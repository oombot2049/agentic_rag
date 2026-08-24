package sse

import (
	"context"
	"errors"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/events"
)

// PollingSubscriber keeps the Gateway available when Redis is unavailable.
// Handler's durable PostgreSQL poll remains the delivery path.
type PollingSubscriber struct{ cause error }

func NewPollingSubscriber(cause error) *PollingSubscriber {
	if cause == nil {
		cause = errors.New("Redis live tail is unavailable")
	}
	return &PollingSubscriber{cause: cause}
}

func (s *PollingSubscriber) Subscribe(context.Context, domain.TenantID, domain.RunID, uint64) (<-chan events.Event, <-chan error) {
	values := make(chan events.Event)
	failures := make(chan error, 1)
	close(values)
	failures <- s.cause
	close(failures)
	return values, failures
}

var _ Subscriber = (*PollingSubscriber)(nil)
