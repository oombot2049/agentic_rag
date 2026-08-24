package searchindex

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/knowledge"
)

type indexGeneration struct {
	records  map[knowledge.ChunkID]Record
	sealed   bool
	manifest Manifest
}
type MemoryAdapter struct {
	mu          sync.RWMutex
	generations map[Scope]*indexGeneration
	active      map[string]knowledge.VersionID
}

func NewMemoryAdapter() *MemoryAdapter {
	return &MemoryAdapter{generations: map[Scope]*indexGeneration{}, active: map[string]knowledge.VersionID{}}
}
func (m *MemoryAdapter) Upsert(_ context.Context, scope Scope, values []Record) error {
	if len(values) < 1 || len(values) > 500 {
		return ErrInvalid
	}
	for _, value := range values {
		if err := ValidateRecord(scope, value); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	generation := m.generations[scope]
	if generation == nil {
		generation = &indexGeneration{records: map[knowledge.ChunkID]Record{}}
		m.generations[scope] = generation
	}
	if generation.sealed {
		return ErrConflict
	}
	for _, value := range values {
		if current, ok := generation.records[value.ChunkID]; ok && RecordDigest(current) != RecordDigest(value) {
			return ErrConflict
		}
	}
	for _, value := range values {
		value.Vector = append([]float32(nil), value.Vector...)
		generation.records[value.ChunkID] = value
	}
	return nil
}
func (m *MemoryAdapter) Seal(_ context.Context, scope Scope, manifest Manifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	generation := m.generations[scope]
	if generation == nil {
		return ErrNotFound
	}
	values := make([]Record, 0, len(generation.records))
	for _, value := range generation.records {
		values = append(values, value)
	}
	actual, err := BuildManifest(values)
	if err != nil || actual != manifest {
		return ErrConflict
	}
	if generation.sealed && generation.manifest != manifest {
		return ErrConflict
	}
	generation.sealed = true
	generation.manifest = manifest
	return nil
}
func (m *MemoryAdapter) Activate(_ context.Context, tenant domain.TenantID, kb knowledge.KnowledgeBaseID, expected, next knowledge.VersionID) error {
	scope := Scope{tenant, kb, next}
	if err := ValidateScope(scope); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	generation := m.generations[scope]
	if generation == nil || !generation.sealed {
		return ErrNotFound
	}
	key := tenant.String() + "\x00" + kb.String()
	current := m.active[key]
	if current == next {
		return nil
	}
	if current != expected {
		return ErrConflict
	}
	m.active[key] = next
	return nil
}
func (m *MemoryAdapter) ResolveActive(_ context.Context, tenant domain.TenantID, kb knowledge.KnowledgeBaseID) (knowledge.VersionID, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.active[tenant.String()+"\x00"+kb.String()]
	if !ok {
		return "", ErrNotFound
	}
	return value, nil
}
func (m *MemoryAdapter) DeleteVersion(_ context.Context, scope Scope, cursor string, limit int) (DeletePage, error) {
	if err := ValidateScope(scope); err != nil {
		return DeletePage{}, err
	}
	if limit < 1 || limit > 1000 {
		return DeletePage{}, ErrInvalid
	}
	offset, err := decodeCursor(cursor)
	if err != nil {
		return DeletePage{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[scope.TenantID.String()+"\x00"+scope.KnowledgeBaseID.String()] == scope.VersionID {
		return DeletePage{}, ErrActiveVersion
	}
	generation := m.generations[scope]
	if generation == nil {
		return DeletePage{}, nil
	}
	ids := make([]knowledge.ChunkID, 0, len(generation.records))
	for id := range generation.records {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if offset > len(ids) {
		return DeletePage{}, ErrInvalid
	}
	end := min(offset+limit, len(ids))
	for _, id := range ids[offset:end] {
		delete(generation.records, id)
	}
	page := DeletePage{Deleted: end - offset}
	if len(generation.records) > 0 {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte("0"))
	} else {
		delete(m.generations, scope)
	}
	return page, nil
}
func decodeCursor(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, ErrInvalid
	}
	result, err := strconv.Atoi(string(raw))
	if err != nil || result < 0 {
		return 0, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return result, nil
}

var _ Adapter = (*MemoryAdapter)(nil)
