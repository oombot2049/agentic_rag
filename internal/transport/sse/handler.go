package sse

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/events"
)

var ErrSlowConsumer = errors.New("SSE consumer is too slow")

type Source interface {
	Bounds(context.Context, domain.TenantID, domain.RunID) (events.Bounds, error)
	Replay(context.Context, domain.TenantID, domain.RunID, uint64, uint64, int) ([]events.Event, error)
}

type Subscriber interface {
	Subscribe(context.Context, domain.TenantID, domain.RunID, uint64) (<-chan events.Event, <-chan error)
}

type Config struct {
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	WriteTimeout      time.Duration
	RetryDelay        time.Duration
	ReplayPageSize    int
	QueueCapacity     int
}

func DefaultConfig() Config {
	return Config{HeartbeatInterval: 15 * time.Second, PollInterval: time.Second, WriteTimeout: 10 * time.Second,
		RetryDelay: 3 * time.Second, ReplayPageSize: 256, QueueCapacity: 64}
}

type Handler struct {
	source     Source
	subscriber Subscriber
	config     Config
	now        func() time.Time
}

func NewHandler(source Source, subscriber Subscriber, configuration Config) (*Handler, error) {
	if source == nil || subscriber == nil || configuration.HeartbeatInterval <= 0 || configuration.PollInterval <= 0 ||
		configuration.WriteTimeout <= 0 || configuration.RetryDelay < time.Second || configuration.RetryDelay > time.Minute ||
		configuration.ReplayPageSize < 1 || configuration.ReplayPageSize > 1000 || configuration.QueueCapacity < 1 || configuration.QueueCapacity > 4096 {
		return nil, errors.New("SSE gateway configuration is invalid")
	}
	return &Handler{source: source, subscriber: subscriber, config: configuration, now: time.Now}, nil
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	read, _ := auth.RequirePermission("sessions:read", nil)
	mux.Handle("GET /v1/runs/{run_id}/events", read(http.HandlerFunc(h.serve)))
	return mux
}

func (h *Handler) serve(response http.ResponseWriter, request *http.Request) {
	principal, ok := auth.PrincipalFromContext(request.Context())
	if !ok {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeUnauthenticated, false, nil))
		return
	}
	if request.URL.RawQuery != "" {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidArgument, false, errors.New("SSE endpoint accepts no query parameters")))
		return
	}
	runID, err := domain.ParseRunID(request.PathValue("run_id"))
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeRunNotFound, false, err))
		return
	}
	after, err := parseLastEventID(request.Header.Values("Last-Event-ID"))
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidEventCursor, false, err))
		return
	}
	bounds, err := h.source.Bounds(request.Context(), principal.TenantID(), runID)
	if err != nil {
		h.writeSourceFailure(response, request, err)
		return
	}
	if after > bounds.HighWater || after > 0 && after+1 < bounds.RetainedFrom {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeEventCursorExpired, false, events.ErrCursorExpired))
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInternal, false, errors.New("streaming response is unsupported")))
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache, no-store, no-transform")
	response.Header().Set("Connection", "keep-alive")
	response.Header().Set("X-Accel-Buffering", "no")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.WriteHeader(http.StatusOK)
	if !h.write(response, func() error { return writeRetry(response, h.config.RetryDelay) }) {
		return
	}
	flusher.Flush()
	last, terminal, ok := h.replay(response, flusher, request, principal.TenantID(), runID, after, bounds.HighWater)
	if !ok || terminal {
		return
	}
	h.live(response, flusher, request, principal.TenantID(), runID, last)
}

func (h *Handler) replay(response http.ResponseWriter, flusher http.Flusher, request *http.Request, tenantID domain.TenantID, runID domain.RunID, after, through uint64) (uint64, bool, bool) {
	last := after
	for last < through {
		batch, err := h.source.Replay(request.Context(), tenantID, runID, last, through, h.config.ReplayPageSize)
		if err != nil || len(batch) == 0 {
			return last, false, false
		}
		for _, event := range batch {
			if event.Sequence() <= last {
				continue
			}
			if event.Sequence() != last+1 || event.RunID() != runID || event.TenantID() != tenantID {
				return last, false, false
			}
			if !h.write(response, func() error { return writeEvent(response, event) }) {
				return last, false, false
			}
			last = event.Sequence()
			flusher.Flush()
			if terminal(event.Type()) {
				return last, true, true
			}
		}
	}
	return last, false, true
}

func (h *Handler) live(response http.ResponseWriter, flusher http.Flusher, request *http.Request, tenantID domain.TenantID, runID domain.RunID, last uint64) {
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	live, failures := h.subscriber.Subscribe(ctx, tenantID, runID, last)
	queue := make(chan events.Event, h.config.QueueCapacity)
	slow := make(chan struct{}, 1)
	go relayLive(ctx, live, queue, slow)
	heartbeat := time.NewTimer(h.config.HeartbeatInterval)
	polls := time.NewTicker(h.config.PollInterval)
	defer heartbeat.Stop()
	defer polls.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case <-slow:
			return
		case <-failures:
			// Redis is an optimization. Durable polling continues recovery.
			failures = nil
		case event, open := <-queue:
			if !open {
				queue = nil
				continue
			}
			if event.Sequence() <= last {
				continue
			}
			if event.Sequence() != last+1 {
				continue // the next durable poll closes any Redis gap
			}
			if !h.write(response, func() error { return writeEvent(response, event) }) {
				return
			}
			last = event.Sequence()
			flusher.Flush()
			resetTimer(heartbeat, h.config.HeartbeatInterval)
			if terminal(event.Type()) {
				return
			}
		case <-polls.C:
			if queue == nil && failures == nil {
				live, nextFailures := h.subscriber.Subscribe(ctx, tenantID, runID, last)
				failures = nextFailures
				queue = make(chan events.Event, h.config.QueueCapacity)
				go relayLive(ctx, live, queue, slow)
			}
			bounds, err := h.source.Bounds(ctx, tenantID, runID)
			if err != nil || bounds.HighWater <= last {
				continue
			}
			var done, ok bool
			last, done, ok = h.replay(response, flusher, request, tenantID, runID, last, bounds.HighWater)
			if !ok || done {
				return
			}
			resetTimer(heartbeat, h.config.HeartbeatInterval)
		case now := <-heartbeat.C:
			if !h.write(response, func() error { return writeHeartbeat(response, now) }) {
				return
			}
			flusher.Flush()
			heartbeat.Reset(h.config.HeartbeatInterval)
		}
	}
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func relayLive(ctx context.Context, input <-chan events.Event, output chan<- events.Event, slow chan<- struct{}) {
	defer close(output)
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-input:
			if !open {
				return
			}
			select {
			case output <- event:
			default:
				select {
				case slow <- struct{}{}:
				default:
				}
				return
			}
		}
	}
}

func (h *Handler) write(response http.ResponseWriter, operation func() error) bool {
	controller := http.NewResponseController(response)
	_ = controller.SetWriteDeadline(h.now().Add(h.config.WriteTimeout))
	return operation() == nil
}

func (h *Handler) writeSourceFailure(response http.ResponseWriter, request *http.Request, err error) {
	if errors.Is(err, events.ErrNotFound) {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeRunNotFound, false, err))
		return
	}
	auth.WriteFailure(response, request, apperror.New(apperror.CodeInternal, false, err))
}

func parseLastEventID(values []string) (uint64, error) {
	if len(values) == 0 {
		return 0, nil
	}
	if len(values) != 1 || values[0] == "" || strings.Contains(values[0], ",") {
		return 0, errors.New("Last-Event-ID must occur at most once")
	}
	value, err := strconv.ParseUint(values[0], 10, 63)
	if err != nil || value > math.MaxInt64 || strconv.FormatUint(value, 10) != values[0] {
		return 0, errors.New("Last-Event-ID must be a canonical non-negative integer")
	}
	return value, nil
}

func terminal(eventType events.Type) bool {
	return eventType == events.RunCompleted || eventType == events.RunFailed || eventType == events.RunCancelled || eventType == events.RunTimedOut
}
