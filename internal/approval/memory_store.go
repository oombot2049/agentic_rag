package approval

import (
	"context"
	"crypto/hmac"
	"sync"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type MemoryStore struct {
	mu      sync.RWMutex
	records map[string]Record
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{records: make(map[string]Record)} }
func (s *MemoryStore) Create(_ context.Context, value Record) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := approvalKey(value.TenantID, value.ID)
	if existing, ok := s.records[key]; ok {
		if !sameRequest(existing, value) {
			return Record{}, ErrConflict
		}
		return existing, nil
	}
	for _, existing := range s.records {
		if existing.TenantID == value.TenantID && existing.RunID == value.RunID && existing.StepID == value.StepID {
			return Record{}, ErrConflict
		}
	}
	s.records[key] = value
	return value, nil
}
func (s *MemoryStore) Decide(_ context.Context, command DecisionCommand) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := approvalKey(command.TenantID, command.ApprovalID)
	value, ok := s.records[key]
	if !ok {
		return Record{}, ErrNotFound
	}
	if value.ApproverSubject != command.ActorSubject || !hmac.Equal(value.NonceHash[:], command.NonceHash[:]) {
		return Record{}, ErrDenied
	}
	target := StatusRejected
	if command.Approve {
		target = StatusApproved
	}
	if value.Status.Terminal() {
		if value.Status == target && value.DecisionActor == command.ActorSubject {
			return value, nil
		}
		return Record{}, ErrConflict
	}
	if value.RowVersion != command.ExpectedRowVersion {
		return Record{}, ErrStale
	}
	if !command.At.Before(value.ExpiresAt) {
		target = StatusExpired
	}
	value.Status, value.DecisionActor, value.DecidedAt = target, command.ActorSubject, command.At.UTC()
	value.RowVersion++
	s.records[key] = value
	return value, nil
}
func (s *MemoryStore) Get(_ context.Context, tenant domain.TenantID, id string) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.records[approvalKey(tenant, id)]
	if !ok {
		return Record{}, ErrNotFound
	}
	return value, nil
}
func approvalKey(tenant domain.TenantID, id string) string { return tenant.String() + "\x00" + id }
