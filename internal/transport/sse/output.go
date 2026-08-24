package sse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/domain"
	platformredis "github.com/agenticrag/agenticrag/internal/platform/redis"
)

const maximumOutputDeltaBytes = 8 << 10

type OutputDelta struct {
	RunID      domain.RunID `json:"run_id"`
	Text       string       `json:"text"`
	OccurredAt time.Time    `json:"occurred_at"`
}

type OutputEntry struct {
	ID    string
	Delta OutputDelta
}

type outputStreamClient interface {
	StreamAppendNext(context.Context, platformredis.Key, []byte, time.Duration) (string, error)
	StreamRead(context.Context, platformredis.Key, string, int64, time.Duration) ([]platformredis.StreamEntry, error)
}

type OutputStream interface {
	SubscribeOutput(context.Context, domain.TenantID, domain.RunID, string) (<-chan OutputEntry, <-chan error)
}

// RedisOutputBroker stores short-lived presentation deltas independently from
// the durable PostgreSQL Run event sequence.
type RedisOutputBroker struct {
	client         outputStreamClient
	keys           platformredis.KeyBuilder
	ttl, readBlock time.Duration
}

func NewRedisOutputBroker(client outputStreamClient, keys platformredis.KeyBuilder, ttl, readBlock time.Duration) (*RedisOutputBroker, error) {
	if client == nil || ttl <= 0 || readBlock < time.Millisecond || readBlock > 30*time.Second {
		return nil, errors.New("SSE output broker configuration is invalid")
	}
	return &RedisOutputBroker{client: client, keys: keys, ttl: ttl, readBlock: readBlock}, nil
}

func (b *RedisOutputBroker) PublishOutput(ctx context.Context, tenantID domain.TenantID, delta OutputDelta) (string, error) {
	if err := validateOutputDelta(delta); err != nil {
		return "", err
	}
	key, err := b.key(tenantID, delta.RunID)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(delta)
	if err != nil {
		return "", err
	}
	return b.client.StreamAppendNext(ctx, key, data, b.ttl)
}

func (b *RedisOutputBroker) SubscribeOutput(ctx context.Context, tenantID domain.TenantID, runID domain.RunID, after string) (<-chan OutputEntry, <-chan error) {
	values, failures := make(chan OutputEntry), make(chan error, 1)
	go func() {
		defer close(values)
		defer close(failures)
		key, err := b.key(tenantID, runID)
		if err != nil {
			failures <- err
			return
		}
		cursor := after
		for {
			entries, err := b.client.StreamRead(ctx, key, cursor, 128, b.readBlock)
			if err != nil {
				if ctx.Err() == nil {
					failures <- err
				}
				return
			}
			for _, entry := range entries {
				var delta OutputDelta
				if json.Unmarshal(entry.Data, &delta) != nil || validateOutputDelta(delta) != nil || delta.RunID != runID {
					failures <- fmt.Errorf("invalid transient output entry %q", entry.ID)
					return
				}
				select {
				case values <- OutputEntry{ID: entry.ID, Delta: delta}:
					cursor = entry.ID
				case <-ctx.Done():
					return
				}
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return values, failures
}

func (b *RedisOutputBroker) key(tenantID domain.TenantID, runID domain.RunID) (platformredis.Key, error) {
	if _, err := domain.ParseTenantID(tenantID.String()); err != nil {
		return platformredis.Key{}, err
	}
	if _, err := domain.ParseRunID(runID.String()); err != nil {
		return platformredis.Key{}, err
	}
	return b.keys.Build(tenantID.String(), platformredis.NamespaceEvents, "output", runID.String())
}

func validateOutputDelta(delta OutputDelta) error {
	if _, err := domain.ParseRunID(delta.RunID.String()); err != nil {
		return err
	}
	if delta.Text == "" || len(delta.Text) > maximumOutputDeltaBytes || !utf8.ValidString(delta.Text) || delta.OccurredAt.IsZero() {
		return errors.New("transient output delta is invalid")
	}
	return nil
}

type OutputHandler struct {
	source Source
	stream OutputStream
	config Config
}

func NewOutputHandler(source Source, stream OutputStream, config Config) (*OutputHandler, error) {
	if source == nil || stream == nil || config.HeartbeatInterval <= 0 || config.WriteTimeout <= 0 {
		return nil, errors.New("SSE output handler configuration is invalid")
	}
	return &OutputHandler{source: source, stream: stream, config: config}, nil
}

func (h *OutputHandler) Routes() http.Handler {
	mux := http.NewServeMux()
	read, _ := auth.RequirePermission("sessions:read", nil)
	mux.Handle("GET /v1/runs/{run_id}/output", read(http.HandlerFunc(h.serve)))
	return mux
}

func (h *OutputHandler) serve(response http.ResponseWriter, request *http.Request) {
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
	if _, err = h.source.Bounds(request.Context(), principal.TenantID(), runID); err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeRunNotFound, false, err))
		return
	}
	after, err := parseOutputCursor(request.Header.Values("Last-Event-ID"))
	if err != nil {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInvalidEventCursor, false, err))
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		auth.WriteFailure(response, request, apperror.New(apperror.CodeInternal, false, errors.New("streaming response is unsupported")))
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache, no-store, no-transform")
	response.Header().Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	if !h.write(response, func() error { return writeRetry(response, h.config.RetryDelay) }) {
		return
	}
	flusher.Flush()
	values, failures := h.stream.SubscribeOutput(request.Context(), principal.TenantID(), runID, after)
	heartbeat := time.NewTicker(h.config.HeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case _, open := <-failures:
			if !open {
				failures = nil
				continue
			}
			return
		case entry, open := <-values:
			if !open {
				return
			}
			data, _ := json.Marshal(entry.Delta)
			if !h.write(response, func() error {
				_, err := fmt.Fprintf(response, "id: %s\nevent: assistant.delta\ndata: %s\n\n", entry.ID, data)
				return err
			}) {
				return
			}
			flusher.Flush()
		case now := <-heartbeat.C:
			if !h.write(response, func() error { return writeHeartbeat(response, now) }) {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *OutputHandler) write(response http.ResponseWriter, operation func() error) bool {
	controller := http.NewResponseController(response)
	_ = controller.SetWriteDeadline(time.Now().Add(h.config.WriteTimeout))
	return operation() == nil
}

func parseOutputCursor(values []string) (string, error) {
	if len(values) == 0 {
		return "0-0", nil
	}
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] || !validOutputCursor(values[0]) {
		return "", errors.New("Last-Event-ID is not a canonical Redis stream ID")
	}
	return values[0], nil
}

func validOutputCursor(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return false
	}
	if parts[1] != "0" || parts[0] == "" || (len(parts[0]) > 1 && parts[0][0] == '0') {
		return false
	}
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
