package outbox

import (
	"context"
	"errors"
	"testing"
)

type publisherFunc func(context.Context, Event) error

func (f publisherFunc) Publish(ctx context.Context, event Event) error { return f(ctx, event) }

func TestRouterDispatchesExactlyOneRegisteredType(t *testing.T) {
	called := 0
	router, err := NewRouter(map[string]Publisher{"RunEvent.v1": publisherFunc(func(_ context.Context, event Event) error {
		called++
		event.Payload[0] = '['
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	event := Event{Type: "RunEvent.v1", Payload: []byte(`{"ok":true}`)}
	if err := router.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if called != 1 || string(event.Payload) != `{"ok":true}` {
		t.Fatalf("called=%d payload=%s", called, event.Payload)
	}
	if err := router.Publish(context.Background(), Event{Type: "Unknown.v1"}); err == nil {
		t.Fatal("unknown outbox type was acknowledged")
	}
}

func TestRouterPreservesPublisherFailure(t *testing.T) {
	want := errors.New("redis unavailable")
	router, _ := NewRouter(map[string]Publisher{"RunEvent.v1": publisherFunc(func(context.Context, Event) error { return want })})
	if err := router.Publish(context.Background(), Event{Type: "RunEvent.v1"}); !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
}
