package events

import (
	"errors"
	"sync"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrSequenceConflict = errors.New("event sequence is not strictly increasing")
	ErrStreamIdentity   = errors.New("event stream identity changed")
)

type streamState struct {
	tenantID domain.TenantID
	turnID   domain.TurnID
	last     uint64
}

// SequenceTracker validates append order. It is not a delivery deduplicator.
type SequenceTracker struct {
	mu      sync.Mutex
	streams map[domain.RunID]streamState
}

func NewSequenceTracker() *SequenceTracker {
	return &SequenceTracker{streams: make(map[domain.RunID]streamState)}
}

func (t *SequenceTracker) Accept(event Event) error {
	if t == nil {
		return errors.New("sequence tracker is nil")
	}
	if _, err := restore(event.schemaVersion, event.eventType, event.tenantID, event.turnID, event.runID,
		event.sequence, event.occurredAt, event.payload, event.known); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	current, exists := t.streams[event.runID]
	if exists {
		if current.tenantID != event.tenantID || current.turnID != event.turnID {
			return ErrStreamIdentity
		}
		if event.sequence <= current.last {
			return ErrSequenceConflict
		}
	}
	t.streams[event.runID] = streamState{tenantID: event.tenantID, turnID: event.turnID, last: event.sequence}
	return nil
}

func (t *SequenceTracker) Forget(runID domain.RunID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.streams, runID)
	t.mu.Unlock()
}
