package audit

import (
	"context"
	"sort"
	"sync"

	"github.com/agenticrag/agenticrag/internal/domain"
)

// MemoryStore is a concurrency-safe reference implementation for tests and local use.
type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]Record
	ids     map[string]struct{}
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]Record), ids: make(map[string]struct{})}
}

func (s *MemoryStore) Append(ctx context.Context, value Record) error {
	if ctx == nil || ctx.Err() != nil || Validate(value) != nil {
		return ErrInvalid
	}
	key := value.TenantID.String() + "\x00" + value.ToolCallID
	s.mu.Lock()
	defer s.mu.Unlock()
	idKey := value.TenantID.String() + "\x00" + value.ID
	if _, exists := s.records[key]; exists {
		return ErrConflict
	}
	if _, exists := s.ids[idKey]; exists {
		return ErrConflict
	}
	s.records[key] = cloneRecord(value)
	s.ids[idKey] = struct{}{}
	return nil
}

func (s *MemoryStore) GetByToolCall(ctx context.Context, tenant domain.TenantID, callID string) (Record, error) {
	if ctx == nil || ctx.Err() != nil {
		return Record{}, ErrInvalid
	}
	s.mu.RLock()
	value, ok := s.records[tenant.String()+"\x00"+callID]
	s.mu.RUnlock()
	if !ok {
		return Record{}, ErrNotFound
	}
	return cloneRecord(value), nil
}

func (s *MemoryStore) ListByRun(ctx context.Context, tenant domain.TenantID, run domain.RunID, after *Cursor, limit int) (Page, error) {
	if ctx == nil || ctx.Err() != nil || limit < 1 {
		return Page{}, ErrInvalid
	}
	s.mu.RLock()
	values := make([]Record, 0)
	for _, value := range s.records {
		if value.TenantID == tenant && value.RunID == run && afterCursor(value, after) {
			values = append(values, cloneRecord(value))
		}
	}
	s.mu.RUnlock()
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].ID < values[j].ID
		}
		return values[i].CreatedAt.Before(values[j].CreatedAt)
	})
	page := Page{}
	if len(values) > limit {
		page.Items = values[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	} else {
		page.Items = values
	}
	return page, nil
}

func afterCursor(value Record, cursor *Cursor) bool {
	return cursor == nil || value.CreatedAt.After(cursor.CreatedAt) ||
		value.CreatedAt.Equal(cursor.CreatedAt) && value.ID > cursor.ID
}

var _ Store = (*MemoryStore)(nil)
