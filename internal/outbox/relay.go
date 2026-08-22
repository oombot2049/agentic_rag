package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type Publisher interface {
	Publish(context.Context, Event) error
}

type RelayConfig struct {
	BatchSize     int
	ClaimDuration time.Duration
	MaxAttempts   int
	RetryBase     time.Duration
	RetryMaximum  time.Duration
}

type Relay struct {
	repository *Repository
	publisher  Publisher
	config     RelayConfig
}

type RelayResult struct {
	Claimed   int
	Published int
	Retried   int
	Dead      int
}

func NewRelay(repository *Repository, publisher Publisher, config RelayConfig) (*Relay, error) {
	if repository == nil || publisher == nil {
		return nil, fmt.Errorf("%w: repository and publisher are required", ErrInvalidArgument)
	}
	if config.BatchSize < 1 || config.BatchSize > 100 ||
		config.ClaimDuration < MinimumClaimDuration || config.ClaimDuration > MaximumClaimDuration ||
		config.MaxAttempts < 1 || config.MaxAttempts > 100 || config.RetryBase <= 0 ||
		config.RetryMaximum < config.RetryBase || config.RetryMaximum > 24*time.Hour {
		return nil, fmt.Errorf("%w: invalid relay configuration", ErrInvalidArgument)
	}
	return &Relay{repository: repository, publisher: publisher, config: config}, nil
}

func (r *Relay) ProcessBatch(ctx context.Context, tenantID domain.TenantID, owner string) (RelayResult, error) {
	events, err := r.repository.ClaimBatch(ctx, ClaimCommand{
		TenantID: tenantID, Owner: owner, Duration: r.config.ClaimDuration, Limit: r.config.BatchSize,
	})
	if err != nil {
		return RelayResult{}, err
	}
	result := RelayResult{Claimed: len(events)}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		publishErr := r.publisher.Publish(ctx, cloneEvent(event))
		ack := Acknowledgement{TenantID: event.TenantID, EventID: event.ID, Owner: event.ClaimOwner, Token: event.ClaimToken}
		if publishErr == nil {
			if err := r.repository.MarkPublished(ctx, ack); err != nil {
				return result, fmt.Errorf("acknowledge published event %s: %w", event.ID, err)
			}
			result.Published++
			continue
		}
		dead := event.AttemptCount >= r.config.MaxAttempts
		failure := Failure{Acknowledgement: ack, RetryAfter: r.retryDelay(event.AttemptCount), Cause: normalizeError(publishErr), Dead: dead}
		if err := r.repository.MarkFailed(ctx, failure); err != nil {
			return result, errors.Join(fmt.Errorf("publish event %s: %w", event.ID, publishErr), fmt.Errorf("record failure: %w", err))
		}
		if dead {
			result.Dead++
		} else {
			result.Retried++
		}
	}
	return result, nil
}

func (r *Relay) retryDelay(attempt int) time.Duration {
	delay := r.config.RetryBase
	for current := 1; current < attempt && delay < r.config.RetryMaximum; current++ {
		if delay > r.config.RetryMaximum/2 {
			return r.config.RetryMaximum
		}
		delay *= 2
	}
	if delay > r.config.RetryMaximum {
		return r.config.RetryMaximum
	}
	return delay
}

func cloneEvent(event Event) Event {
	event.Payload = append([]byte(nil), event.Payload...)
	return event
}

func normalizeError(err error) string {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "publisher returned an empty error"
	}
	for len(message) > MaximumErrorLength {
		_, size := utf8.DecodeLastRuneInString(message)
		message = message[:len(message)-size]
	}
	return message
}
