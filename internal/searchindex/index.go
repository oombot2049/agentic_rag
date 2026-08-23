// Package searchindex defines vendor-neutral versioned hybrid index writes.
package searchindex

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

var (
	ErrInvalid       = errors.New("invalid search index input")
	ErrConflict      = errors.New("search index conflict")
	ErrNotFound      = errors.New("search index version not found")
	ErrActiveVersion = errors.New("active search index version")
)

type Scope struct {
	TenantID        domain.TenantID
	KnowledgeBaseID knowledge.KnowledgeBaseID
	VersionID       knowledge.VersionID
}
type Record struct {
	DocumentID                   knowledge.DocumentID
	ChunkID                      knowledge.ChunkID
	Content, Model, ModelVersion string
	ContentHash                  [sha256.Size]byte
	Vector                       []float32
	ACL                          knowledge.ACL
}
type Manifest struct {
	RecordCount uint64
	Digest      [sha256.Size]byte
}
type DeletePage struct {
	Deleted    int
	NextCursor string
}
type Adapter interface {
	Upsert(context.Context, Scope, []Record) error
	Seal(context.Context, Scope, Manifest) error
	Activate(context.Context, domain.TenantID, knowledge.KnowledgeBaseID, knowledge.VersionID, knowledge.VersionID) error
	ResolveActive(context.Context, domain.TenantID, knowledge.KnowledgeBaseID) (knowledge.VersionID, error)
	DeleteVersion(context.Context, Scope, string, int) (DeletePage, error)
}

func ValidateScope(value Scope) error {
	if _, err := domain.ParseTenantID(value.TenantID.String()); err != nil {
		return err
	}
	if _, err := knowledge.ParseKnowledgeBaseID(value.KnowledgeBaseID.String()); err != nil {
		return err
	}
	if _, err := knowledge.ParseVersionID(value.VersionID.String()); err != nil {
		return err
	}
	return nil
}
func ValidateRecord(scope Scope, value Record) error {
	if err := ValidateScope(scope); err != nil {
		return err
	}
	if _, err := knowledge.ParseDocumentID(value.DocumentID.String()); err != nil {
		return err
	}
	if _, err := knowledge.ParseChunkID(value.ChunkID.String()); err != nil {
		return err
	}
	if strings.TrimSpace(value.Content) == "" || sha256.Sum256([]byte(value.Content)) != value.ContentHash || value.ACL.TenantID() != scope.TenantID || value.Model == "" || value.ModelVersion == "" || len(value.Vector) == 0 || len(value.Vector) > 65536 {
		return ErrInvalid
	}
	for _, number := range value.Vector {
		if math.IsNaN(float64(number)) || math.IsInf(float64(number), 0) {
			return ErrInvalid
		}
	}
	return nil
}
func RecordDigest(value Record) [sha256.Size]byte {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%x\x00%s\x00%s\x00%d\x00", value.DocumentID, value.ChunkID, value.ContentHash, value.Model, value.ModelVersion, value.ACL.Revision())
	for _, grant := range value.ACL.Grants() {
		fmt.Fprintf(hash, "%s:%s:%d\x00", grant.Kind, grant.PrincipalID, grant.Permission)
	}
	for _, number := range value.Vector {
		fmt.Fprintf(hash, "%08x", math.Float32bits(number))
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}
func BuildManifest(values []Record) (Manifest, error) {
	if len(values) == 0 {
		return Manifest{}, ErrInvalid
	}
	type item struct {
		id     string
		digest [sha256.Size]byte
	}
	items := make([]item, len(values))
	seen := map[knowledge.ChunkID]struct{}{}
	for index, value := range values {
		if _, ok := seen[value.ChunkID]; ok {
			return Manifest{}, ErrConflict
		}
		seen[value.ChunkID] = struct{}{}
		items[index] = item{value.ChunkID.String(), RecordDigest(value)}
	}
	slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.id, b.id) })
	hash := sha256.New()
	for _, value := range items {
		fmt.Fprintf(hash, "%s\x00%x", value.id, value.digest)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return Manifest{uint64(len(items)), digest}, nil
}
