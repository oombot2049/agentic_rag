package sse

import (
	"context"
	"errors"
	"fmt"

	"github.com/agenticrag/agenticrag/internal/events"
	"github.com/agenticrag/agenticrag/internal/outbox"
)

// OutboxPublisher converts a durable RunEvent.v1 delivery into the transient
// Redis live tail. PostgreSQL remains authoritative when publication fails.
type OutboxPublisher struct{ broker *RedisBroker }

func NewOutboxPublisher(broker *RedisBroker) (*OutboxPublisher, error) {
	if broker == nil {
		return nil, errors.New("SSE outbox publisher requires a Redis broker")
	}
	return &OutboxPublisher{broker: broker}, nil
}

func (p *OutboxPublisher) Publish(ctx context.Context, delivery outbox.Event) error {
	if delivery.Type != "RunEvent.v1" || delivery.AggregateType != "run" ||
		delivery.Status != outbox.StatusPending || delivery.AggregateID == "" {
		return errors.New("invalid RunEvent.v1 outbox delivery")
	}
	event, err := events.DecodeStrict(delivery.TenantID, delivery.Payload)
	if err != nil {
		return fmt.Errorf("decode RunEvent.v1 delivery: %w", err)
	}
	if event.RunID().String() != delivery.AggregateID {
		return errors.New("decode RunEvent.v1 delivery: aggregate identity mismatch")
	}
	return p.broker.Publish(ctx, event)
}

var _ outbox.Publisher = (*OutboxPublisher)(nil)
