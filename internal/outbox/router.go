package outbox

import (
	"context"
	"fmt"
)

// Router sends each outbox event type to exactly one publisher. Unknown types
// fail closed and are retried/dead-lettered by Relay instead of being acked.
type Router struct{ publishers map[string]Publisher }

func NewRouter(publishers map[string]Publisher) (*Router, error) {
	if len(publishers) == 0 {
		return nil, fmt.Errorf("%w: at least one outbox route is required", ErrInvalidArgument)
	}
	copyRoutes := make(map[string]Publisher, len(publishers))
	for eventType, publisher := range publishers {
		if !namePattern.MatchString(eventType) || publisher == nil {
			return nil, fmt.Errorf("%w: invalid outbox route %q", ErrInvalidArgument, eventType)
		}
		copyRoutes[eventType] = publisher
	}
	return &Router{publishers: copyRoutes}, nil
}

func (r *Router) Publish(ctx context.Context, event Event) error {
	publisher, ok := r.publishers[event.Type]
	if !ok {
		return fmt.Errorf("no publisher registered for outbox event type %q", event.Type)
	}
	return publisher.Publish(ctx, cloneEvent(event))
}
