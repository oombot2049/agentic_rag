package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var artifactTime = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

type memoryMetadata struct {
	value     Metadata
	insertErr error
}

func (r *memoryMetadata) Insert(_ context.Context, value Metadata) error {
	if r.insertErr != nil {
		return r.insertErr
	}
	r.value = value
	return nil
}
func (r *memoryMetadata) Get(_ context.Context, tenantID domain.TenantID, id string) (Metadata, error) {
	if r.value.TenantID != tenantID || r.value.ID != id {
		return Metadata{}, ErrNotFound
	}
	return r.value, nil
}

type memoryStore struct {
	mu        sync.Mutex
	objects   map[string][]byte
	putInfo   *ObjectInfo
	putErr    error
	openErr   error
	deleteErr error
	deletes   int
}

func (s *memoryStore) Put(_ context.Context, key string, source io.Reader, _ int64, options PutOptions) (ObjectInfo, error) {
	if s.putErr != nil {
		return ObjectInfo{}, s.putErr
	}
	if !options.CreateOnly {
		return ObjectInfo{}, errors.New("create-only contract lost")
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return ObjectInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objects[key]; exists {
		return ObjectInfo{}, ErrAlreadyExists
	}
	s.objects[key] = data
	if s.putInfo != nil {
		return *s.putInfo, nil
	}
	hash := sha256.Sum256(data)
	return ObjectInfo{Size: int64(len(data)), SHA256: &hash}, nil
}
func (s *memoryStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, exists := s.objects[key]
	if !exists {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}
func (s *memoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	delete(s.objects, key)
	return s.deleteErr
}

func TestServiceStreamsUploadAndVerifiedDownload(t *testing.T) {
	t.Parallel()
	repository := &memoryMetadata{}
	store := &memoryStore{objects: map[string][]byte{}}
	service, err := NewService(repository, store)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return artifactTime }
	payload := bytes.Repeat([]byte("streaming-payload-"), 1<<16)
	metadata, err := service.Upload(context.Background(), Upload{TenantID: "ten_0001", ID: "art_0001",
		Size: int64(len(payload)), MediaType: "application/octet-stream", EncryptionMode: EncryptionSSES3,
		TTL: 24 * time.Hour, Body: bytes.NewReader(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ObjectKey != "tenants/ten_0001/artifacts/art_0001" || metadata.SHA256 != sha256.Sum256(payload) {
		t.Fatalf("metadata = %+v", metadata)
	}
	_, reader, err := service.Download(context.Background(), "ten_0001", "art_0001")
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(downloaded, payload) {
		t.Fatalf("download bytes=%d error=%v", len(downloaded), err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRejectsLengthChecksumExpiryAndRollsBack(t *testing.T) {
	t.Parallel()
	badHash := sha256.Sum256([]byte("wrong"))
	for _, test := range []struct {
		name    string
		body    []byte
		size    int64
		info    *ObjectInfo
		insert  error
		want    error
		deletes int
	}{
		{"short", []byte("short"), 10, nil, nil, ErrIntegrity, 1},
		{"extra", []byte("too long"), 3, &ObjectInfo{Size: 3}, nil, ErrIntegrity, 1},
		{"checksum", []byte("right"), 5, &ObjectInfo{Size: 5, SHA256: &badHash}, nil, ErrIntegrity, 1},
		{"metadata", []byte("right"), 5, nil, errors.New("database failed"), errors.New("database failed"), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &memoryMetadata{insertErr: test.insert}
			store := &memoryStore{objects: map[string][]byte{}, putInfo: test.info}
			service, _ := NewService(repository, store)
			service.now = func() time.Time { return artifactTime }
			_, err := service.Upload(context.Background(), Upload{TenantID: "ten_0001", ID: "art_0001", Size: test.size,
				MediaType: "text/plain", EncryptionMode: EncryptionSSES3, TTL: time.Hour, Body: bytes.NewReader(test.body)})
			matches := errors.Is(err, test.want) || (err != nil && strings.Contains(err.Error(), test.want.Error()))
			if !matches || store.deletes != test.deletes {
				t.Fatalf("error=%v deletes=%d", err, store.deletes)
			}
		})
	}
	repository := &memoryMetadata{}
	store := &memoryStore{objects: map[string][]byte{}}
	service, _ := NewService(repository, store)
	service.now = func() time.Time { return artifactTime }
	_, _ = service.Upload(context.Background(), Upload{TenantID: "ten_0001", ID: "art_0001", Size: 1,
		MediaType: "text/plain", EncryptionMode: EncryptionSSES3, TTL: time.Hour, Body: bytes.NewReader([]byte("x"))})
	service.now = func() time.Time { return artifactTime.Add(time.Hour) }
	if _, _, err := service.Download(context.Background(), "ten_0001", "art_0001"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired download error = %v", err)
	}
}

func TestVerifiedDownloadDetectsCorruption(t *testing.T) {
	t.Parallel()
	want := []byte("expected")
	repository := &memoryMetadata{value: Metadata{TenantID: "ten_0001", ID: "art_0001",
		ObjectKey: "tenants/ten_0001/artifacts/art_0001", SHA256: sha256.Sum256(want), Size: int64(len(want)),
		MediaType: "text/plain", EncryptionMode: EncryptionSSES3, CreatedAt: artifactTime, ExpiresAt: artifactTime.Add(time.Hour)}}
	store := &memoryStore{objects: map[string][]byte{repository.value.ObjectKey: []byte("corrupt!")}}
	service, _ := NewService(repository, store)
	service.now = func() time.Time { return artifactTime }
	_, reader, err := service.Download(context.Background(), "ten_0001", "art_0001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corruption error = %v", err)
	}
}

func TestArtifactValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewRepository(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil repository = %v", err)
	}
	if _, err := NewService(nil, &memoryStore{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil service = %v", err)
	}
	service, _ := NewService(&memoryMetadata{}, &memoryStore{objects: map[string][]byte{}})
	service.now = func() time.Time { return artifactTime }
	for _, upload := range []Upload{
		{}, {TenantID: "ten_0001", ID: "bad", Size: 1, MediaType: "text/plain", EncryptionMode: EncryptionSSES3, TTL: time.Hour, Body: strings.NewReader("x")},
		{TenantID: "ten_0001", ID: "art_0001", Size: 1, MediaType: "bad media ;", EncryptionMode: EncryptionSSES3, TTL: time.Hour, Body: strings.NewReader("x")},
		{TenantID: "ten_0001", ID: "art_0001", Size: 1, MediaType: "text/plain", EncryptionMode: EncryptionSSEKMS, TTL: time.Hour, Body: strings.NewReader("x")},
	} {
		if _, err := service.Upload(context.Background(), upload); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("validation error = %v", err)
		}
	}
}

func TestRepositoryInsertGetTenantScopeAndCorruption(t *testing.T) {
	t.Parallel()
	hash := sha256.Sum256([]byte("payload"))
	row := []driver.Value{"ten_0001", "art_0001", "tenants/ten_0001/artifacts/art_0001", hash[:], int64(7),
		"text/plain", "SSE-S3", nil, artifactTime, artifactTime.Add(time.Hour)}
	script := &artifactSQLScript{execs: []artifactExec{{affected: 1}}, queries: []artifactQuery{{rows: [][]driver.Value{row}}}}
	repository := openArtifactRepository(t, script)
	metadata := Metadata{TenantID: "ten_0001", ID: "art_0001", ObjectKey: "tenants/ten_0001/artifacts/art_0001",
		SHA256: hash, Size: 7, MediaType: "text/plain", EncryptionMode: EncryptionSSES3,
		CreatedAt: artifactTime, ExpiresAt: artifactTime.Add(time.Hour)}
	if err := repository.Insert(context.Background(), metadata); err != nil {
		t.Fatal(err)
	}
	got, err := repository.Get(context.Background(), "ten_0001", "art_0001")
	if err != nil || got.SHA256 != hash || got.ObjectKey != metadata.ObjectKey {
		t.Fatalf("Get = %+v/%v", got, err)
	}
	script.mu.Lock()
	defer script.mu.Unlock()
	if len(script.calls) != 2 || !strings.Contains(script.calls[1], "tenant_id=$1") || strings.Contains(script.calls[0], "payload") {
		t.Fatalf("repository SQL = %v", script.calls)
	}
}

func TestRepositoryNotFoundErrorsAndCorruptHash(t *testing.T) {
	t.Parallel()
	repository := openArtifactRepository(t, &artifactSQLScript{queries: []artifactQuery{{}}})
	if _, err := repository.Get(context.Background(), "ten_0001", "art_0001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not found error = %v", err)
	}
	corrupt := []driver.Value{"ten_0001", "art_0001", "tenants/ten_0001/artifacts/art_0001", []byte{1}, int64(1),
		"text/plain", "SSE-S3", nil, artifactTime, artifactTime.Add(time.Hour)}
	repository = openArtifactRepository(t, &artifactSQLScript{queries: []artifactQuery{{rows: [][]driver.Value{corrupt}}}})
	if _, err := repository.Get(context.Background(), "ten_0001", "art_0001"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt hash error = %v", err)
	}
	databaseError := errors.New("database marker")
	repository = openArtifactRepository(t, &artifactSQLScript{execs: []artifactExec{{err: databaseError}}})
	hash := sha256.Sum256([]byte("x"))
	err := repository.Insert(context.Background(), Metadata{TenantID: "ten_0001", ID: "art_0001", ObjectKey: "tenants/ten_0001/artifacts/art_0001",
		SHA256: hash, Size: 1, MediaType: "text/plain", EncryptionMode: EncryptionSSES3, CreatedAt: artifactTime, ExpiresAt: artifactTime.Add(time.Hour)})
	if !errors.Is(err, databaseError) {
		t.Fatalf("database error = %v", err)
	}
}

type artifactQuery struct {
	rows [][]driver.Value
	err  error
}
type artifactExec struct {
	affected int64
	err      error
}
type artifactSQLScript struct {
	mu      sync.Mutex
	queries []artifactQuery
	execs   []artifactExec
	calls   []string
}

var artifactDriverSequence atomic.Uint64

func openArtifactRepository(t *testing.T, script *artifactSQLScript) *Repository {
	t.Helper()
	name := fmt.Sprintf("artifact-script-%d", artifactDriverSequence.Add(1))
	sql.Register(name, artifactDriver{script})
	database, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repository, err := NewRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

type artifactDriver struct{ script *artifactSQLScript }

func (d artifactDriver) Open(string) (driver.Conn, error) { return &artifactConn{d.script}, nil }

type artifactConn struct{ script *artifactSQLScript }

func (c *artifactConn) Prepare(string) (driver.Stmt, error)      { return nil, errors.New("unsupported") }
func (c *artifactConn) Close() error                             { return nil }
func (c *artifactConn) Begin() (driver.Tx, error)                { return nil, errors.New("unsupported") }
func (c *artifactConn) CheckNamedValue(*driver.NamedValue) error { return nil }
func (c *artifactConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	c.script.calls = append(c.script.calls, query)
	if len(c.script.execs) == 0 {
		return nil, errors.New("unexpected exec")
	}
	result := c.script.execs[0]
	c.script.execs = c.script.execs[1:]
	if result.err != nil {
		return nil, result.err
	}
	return driver.RowsAffected(result.affected), nil
}
func (c *artifactConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	c.script.calls = append(c.script.calls, query)
	if len(c.script.queries) == 0 {
		return nil, errors.New("unexpected query")
	}
	result := c.script.queries[0]
	c.script.queries = c.script.queries[1:]
	if result.err != nil {
		return nil, result.err
	}
	return &artifactRows{rows: result.rows}, nil
}

type artifactRows struct {
	rows  [][]driver.Value
	index int
}

func (r *artifactRows) Columns() []string {
	count := 0
	if len(r.rows) > 0 {
		count = len(r.rows[0])
	}
	columns := make([]string, count)
	for index := range columns {
		columns[index] = fmt.Sprintf("column_%d", index)
	}
	return columns
}
func (r *artifactRows) Close() error { return nil }
func (r *artifactRows) Next(destination []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(destination, r.rows[r.index])
	r.index++
	return nil
}
