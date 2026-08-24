// Package artifact provides immutable, streaming Artifact persistence.
package artifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"regexp"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

const MaximumSize int64 = 5 << 40

var (
	ErrInvalidArgument = errors.New("artifact invalid argument")
	ErrNotFound        = errors.New("artifact not found")
	ErrExpired         = errors.New("artifact expired")
	ErrIntegrity       = errors.New("artifact integrity check failed")
	ErrAlreadyExists   = errors.New("artifact already exists")
	artifactIDPattern  = regexp.MustCompile(`^art_[A-Za-z0-9_-]{4,124}$`)
)

type EncryptionMode string

const (
	EncryptionSSES3  EncryptionMode = "SSE-S3"
	EncryptionSSEKMS EncryptionMode = "SSE-KMS"
	EncryptionClient EncryptionMode = "CLIENT"
)

type Metadata struct {
	TenantID       domain.TenantID
	ID             string
	ObjectKey      string
	SHA256         [sha256.Size]byte
	Size           int64
	MediaType      string
	EncryptionMode EncryptionMode
	EncryptionKey  string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Repository struct{ db DBTX }

func NewRepository(db DBTX) (*Repository, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: database executor is nil", ErrInvalidArgument)
	}
	return &Repository{db: db}, nil
}

func (r *Repository) Insert(ctx context.Context, metadata Metadata) error {
	if err := validateMetadata(metadata); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx, insertMetadataSQL, metadata.TenantID.String(), metadata.ID,
		metadata.ObjectKey, metadata.SHA256[:], metadata.Size, metadata.MediaType,
		metadata.EncryptionMode, optionalKey(metadata), metadata.CreatedAt.UTC(), metadata.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert artifact metadata: %w", err)
	}
	return nil
}

func (r *Repository) Get(ctx context.Context, tenantID domain.TenantID, artifactID string) (Metadata, error) {
	if err := validateIdentity(tenantID, artifactID); err != nil {
		return Metadata{}, err
	}
	var metadata Metadata
	var rawTenant, rawMode string
	var hash []byte
	var key sql.NullString
	err := r.db.QueryRowContext(ctx, selectMetadataSQL, tenantID.String(), artifactID).Scan(
		&rawTenant, &metadata.ID, &metadata.ObjectKey, &hash, &metadata.Size, &metadata.MediaType,
		&rawMode, &key, &metadata.CreatedAt, &metadata.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Metadata{}, ErrNotFound
	}
	if err != nil {
		return Metadata{}, fmt.Errorf("select artifact metadata: %w", err)
	}
	parsedTenant, err := domain.ParseTenantID(rawTenant)
	if err != nil || len(hash) != sha256.Size {
		return Metadata{}, fmt.Errorf("%w: corrupt stored identity or hash", ErrIntegrity)
	}
	metadata.TenantID = parsedTenant
	metadata.EncryptionMode = EncryptionMode(rawMode)
	metadata.EncryptionKey = key.String
	copy(metadata.SHA256[:], hash)
	metadata.CreatedAt = metadata.CreatedAt.UTC()
	metadata.ExpiresAt = metadata.ExpiresAt.UTC()
	if err := validateMetadata(metadata); err != nil {
		return Metadata{}, fmt.Errorf("hydrate artifact metadata: %w", err)
	}
	return metadata, nil
}

type PutOptions struct {
	MediaType      string
	EncryptionMode EncryptionMode
	EncryptionKey  string
	CreateOnly     bool
}

type ObjectInfo struct {
	Size   int64
	SHA256 *[sha256.Size]byte
}

type ObjectStore interface {
	Put(context.Context, string, io.Reader, int64, PutOptions) (ObjectInfo, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

type MetadataRepository interface {
	Insert(context.Context, Metadata) error
	Get(context.Context, domain.TenantID, string) (Metadata, error)
}

type Service struct {
	repository MetadataRepository
	store      ObjectStore
	now        func() time.Time
}

func NewService(repository MetadataRepository, store ObjectStore) (*Service, error) {
	if repository == nil || store == nil {
		return nil, fmt.Errorf("%w: repository and object store are required", ErrInvalidArgument)
	}
	return &Service{repository: repository, store: store, now: time.Now}, nil
}

type Upload struct {
	TenantID       domain.TenantID
	ID             string
	Size           int64
	MediaType      string
	EncryptionMode EncryptionMode
	EncryptionKey  string
	TTL            time.Duration
	Body           io.Reader
}

func (s *Service) Upload(ctx context.Context, upload Upload) (Metadata, error) {
	createdAt := s.now().UTC()
	metadata := Metadata{TenantID: upload.TenantID, ID: upload.ID, ObjectKey: objectKey(upload.TenantID, upload.ID),
		Size: upload.Size, MediaType: upload.MediaType, EncryptionMode: upload.EncryptionMode,
		EncryptionKey: upload.EncryptionKey, CreatedAt: createdAt, ExpiresAt: createdAt.Add(upload.TTL)}
	if upload.Body == nil {
		return Metadata{}, fmt.Errorf("%w: body is required", ErrInvalidArgument)
	}
	if err := validateMetadata(metadata); err != nil {
		return Metadata{}, err
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: upload.Body, N: upload.Size + 1}
	info, err := s.store.Put(ctx, metadata.ObjectKey, io.TeeReader(limited, hash), upload.Size, PutOptions{
		MediaType: upload.MediaType, EncryptionMode: upload.EncryptionMode, EncryptionKey: upload.EncryptionKey, CreateOnly: true,
	})
	if err != nil {
		return Metadata{}, fmt.Errorf("put artifact object: %w", err)
	}
	var probe [1]byte
	probeCount, probeErr := limited.Read(probe[:])
	if probeErr != nil && !errors.Is(probeErr, io.EOF) {
		return Metadata{}, s.rollbackObject(ctx, metadata.ObjectKey, fmt.Errorf("probe artifact length: %w", probeErr))
	}
	if limited.N != 1 || info.Size != upload.Size {
		return Metadata{}, s.rollbackObject(ctx, metadata.ObjectKey, fmt.Errorf("%w: declared=%d provider=%d remaining=%d extra=%d", ErrIntegrity, upload.Size, info.Size, limited.N, probeCount))
	}
	copy(metadata.SHA256[:], hash.Sum(nil))
	if info.SHA256 != nil && *info.SHA256 != metadata.SHA256 {
		return Metadata{}, s.rollbackObject(ctx, metadata.ObjectKey, fmt.Errorf("%w: provider checksum mismatch", ErrIntegrity))
	}
	if err := s.repository.Insert(ctx, metadata); err != nil {
		return Metadata{}, s.rollbackObject(ctx, metadata.ObjectKey, err)
	}
	return metadata, nil
}

func (s *Service) Download(ctx context.Context, tenantID domain.TenantID, artifactID string) (Metadata, io.ReadCloser, error) {
	metadata, err := s.repository.Get(ctx, tenantID, artifactID)
	if err != nil {
		return Metadata{}, nil, err
	}
	if !metadata.ExpiresAt.After(s.now().UTC()) {
		return Metadata{}, nil, ErrExpired
	}
	reader, err := s.store.Open(ctx, metadata.ObjectKey)
	if err != nil {
		return Metadata{}, nil, fmt.Errorf("open artifact object: %w", err)
	}
	return metadata, &verifyingReader{source: reader, expectedHash: metadata.SHA256, expectedSize: metadata.Size, hash: sha256.New()}, nil
}

func (s *Service) rollbackObject(ctx context.Context, key string, cause error) error {
	return errors.Join(cause, s.store.Delete(ctx, key))
}

type verifyingReader struct {
	source io.ReadCloser
	hash   interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
	expectedHash [sha256.Size]byte
	expectedSize int64
	read         int64
	verified     bool
}

func (r *verifyingReader) Read(destination []byte) (int, error) {
	count, err := r.source.Read(destination)
	if count > 0 {
		r.read += int64(count)
		_, _ = r.hash.Write(destination[:count])
	}
	if errors.Is(err, io.EOF) && !r.verified {
		r.verified = true
		var actual [sha256.Size]byte
		copy(actual[:], r.hash.Sum(nil))
		if r.read != r.expectedSize || actual != r.expectedHash {
			return count, fmt.Errorf("%w: downloaded object differs from metadata", ErrIntegrity)
		}
	}
	return count, err
}

func (r *verifyingReader) Close() error { return r.source.Close() }

func validateMetadata(metadata Metadata) error {
	if err := validateIdentity(metadata.TenantID, metadata.ID); err != nil {
		return err
	}
	if metadata.ObjectKey != objectKey(metadata.TenantID, metadata.ID) {
		return fmt.Errorf("%w: object key is not server-generated", ErrInvalidArgument)
	}
	if metadata.Size < 0 || metadata.Size > MaximumSize {
		return fmt.Errorf("%w: size is outside 0..5 TiB", ErrInvalidArgument)
	}
	if len(metadata.MediaType) < 1 || len(metadata.MediaType) > 255 {
		return fmt.Errorf("%w: media type length is invalid", ErrInvalidArgument)
	}
	if parsed, _, err := mime.ParseMediaType(metadata.MediaType); err != nil || parsed == "" {
		return fmt.Errorf("%w: media type is invalid", ErrInvalidArgument)
	}
	if metadata.EncryptionMode != EncryptionSSES3 && metadata.EncryptionMode != EncryptionSSEKMS && metadata.EncryptionMode != EncryptionClient {
		return fmt.Errorf("%w: encryption mode is invalid", ErrInvalidArgument)
	}
	needsKey := metadata.EncryptionMode == EncryptionSSEKMS || metadata.EncryptionMode == EncryptionClient
	if needsKey != (metadata.EncryptionKey != "") || len(metadata.EncryptionKey) > 512 {
		return fmt.Errorf("%w: encryption key metadata is invalid", ErrInvalidArgument)
	}
	if metadata.CreatedAt.IsZero() || !metadata.ExpiresAt.After(metadata.CreatedAt) || metadata.ExpiresAt.After(metadata.CreatedAt.AddDate(10, 0, 0)) {
		return fmt.Errorf("%w: TTL must be positive and at most ten years", ErrInvalidArgument)
	}
	return nil
}

func validateIdentity(tenantID domain.TenantID, artifactID string) error {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil || !artifactIDPattern.MatchString(artifactID) {
		return fmt.Errorf("%w: tenant or Artifact ID is invalid", ErrInvalidArgument)
	}
	return nil
}

func objectKey(tenantID domain.TenantID, artifactID string) string {
	return "tenants/" + tenantID.String() + "/artifacts/" + artifactID
}

func optionalKey(metadata Metadata) any {
	if strings.TrimSpace(metadata.EncryptionKey) == "" {
		return nil
	}
	return metadata.EncryptionKey
}

const insertMetadataSQL = `INSERT INTO artifact_metadata(
tenant_id,id,object_key,content_sha256,size_bytes,media_type,encryption_mode,encryption_key_id,created_at,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`

const selectMetadataSQL = `SELECT tenant_id,id,object_key,content_sha256,size_bytes,media_type,
encryption_mode,encryption_key_id,created_at,expires_at FROM artifact_metadata WHERE tenant_id=$1 AND id=$2`
