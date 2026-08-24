package tools

import (
	"context"
	"sync"

	"github.com/agenticrag/agenticrag/internal/domain"
)

// MemoryLedgerStore is a concurrency-safe reference adapter for contract tests
// and local development. PostgreSQL remains the production source of truth.
type MemoryLedgerStore struct {
	mu    sync.RWMutex
	calls map[string]CallRecord
}

func NewMemoryLedgerStore() *MemoryLedgerStore {
	return &MemoryLedgerStore{calls: make(map[string]CallRecord)}
}

func (s *MemoryLedgerStore) Begin(_ context.Context, call CallRecord, attempt AttemptRecord) (CallRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ledgerKey(call.TenantID, call.CallID)
	current, exists := s.calls[key]
	if !exists {
		call.Attempts = []AttemptRecord{attempt}
		s.calls[key] = cloneCallRecord(call)
		return cloneCallRecord(call), true, nil
	}
	if !sameImmutableCall(current, call) {
		return CallRecord{}, false, ErrLedgerConflict
	}
	for _, existing := range current.Attempts {
		if existing.AttemptID == attempt.AttemptID {
			if existing.AttemptNo != attempt.AttemptNo {
				return CallRecord{}, false, ErrLedgerConflict
			}
			return cloneCallRecord(current), false, nil
		}
		if existing.AttemptNo == attempt.AttemptNo {
			return CallRecord{}, false, ErrLedgerConflict
		}
	}
	if current.Status.Terminal() {
		return cloneCallRecord(current), false, nil
	}
	if attempt.AttemptNo != current.Attempts[len(current.Attempts)-1].AttemptNo+1 {
		return CallRecord{}, false, ErrLedgerConflict
	}
	if attempt.StartedAt.Before(current.UpdatedAt) {
		return CallRecord{}, false, ErrLedgerConflict
	}
	last := len(current.Attempts) - 1
	if current.Attempts[last].Status == domain.AttemptStatusStarted {
		current.Attempts[last].Status = domain.AttemptStatusUnknown
		current.Attempts[last].EndedAt = attempt.StartedAt
	}
	current.Status = CallUnknown
	current.Attempts = append(current.Attempts, attempt)
	current.UpdatedAt = attempt.StartedAt
	current.RowVersion++
	s.calls[key] = cloneCallRecord(current)
	return cloneCallRecord(current), false, nil
}

func (s *MemoryLedgerStore) RecordOutcome(_ context.Context, command OutcomeCommand) (CallRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ledgerKey(command.TenantID, command.CallID)
	current, exists := s.calls[key]
	if !exists {
		return CallRecord{}, ErrLedgerNotFound
	}
	if current.RowVersion != command.ExpectedRowVersion || current.Status.Terminal() {
		return CallRecord{}, ErrLedgerStale
	}
	index := -1
	for candidate := range current.Attempts {
		if current.Attempts[candidate].AttemptID == command.AttemptID {
			index = candidate
			break
		}
	}
	if index < 0 || current.Attempts[index].Status != domain.AttemptStatusStarted {
		return CallRecord{}, ErrLedgerStale
	}
	if command.At.UTC().Before(current.Attempts[index].StartedAt) {
		return CallRecord{}, ErrLedgerInvalid
	}
	current.Status = command.Status
	current.Result = append(current.Result[:0], command.Result...)
	current.FailureClass = command.FailureClass
	current.RowVersion++
	current.UpdatedAt = command.At.UTC()
	current.Attempts[index].EndedAt = command.At.UTC()
	switch command.Status {
	case CallSucceeded:
		current.Attempts[index].Status = domain.AttemptStatusSucceeded
	case CallFailed:
		current.Attempts[index].Status = domain.AttemptStatusFailed
	case CallUnknown:
		current.Attempts[index].Status = domain.AttemptStatusUnknown
	}
	s.calls[key] = cloneCallRecord(current)
	return cloneCallRecord(current), nil
}

func (s *MemoryLedgerStore) Get(_ context.Context, tenantID domain.TenantID, callID string) (CallRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, exists := s.calls[ledgerKey(tenantID, callID)]
	if !exists {
		return CallRecord{}, ErrLedgerNotFound
	}
	return cloneCallRecord(value), nil
}

func ledgerKey(tenantID domain.TenantID, callID string) string {
	return tenantID.String() + "\x00" + callID
}
