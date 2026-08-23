package sse

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/events"
)

type memorySource struct {
	mu       sync.Mutex
	tenant   domain.TenantID
	values   []events.Event
	retained uint64
	err      error
}

func (s *memorySource) Bounds(_ context.Context, tenant domain.TenantID, _ domain.RunID) (events.Bounds, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenant = tenant
	if s.err != nil {
		return events.Bounds{}, s.err
	}
	high := uint64(0)
	if len(s.values) > 0 {
		high = s.values[len(s.values)-1].Sequence()
	}
	retained := s.retained
	if retained == 0 {
		retained = 1
	}
	return events.Bounds{SessionID: "ses_0001", TurnID: "trn_0001", RetainedFrom: retained, HighWater: high}, nil
}

func (s *memorySource) Replay(_ context.Context, _ domain.TenantID, _ domain.RunID, after, through uint64, limit int) ([]events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]events.Event, 0, limit)
	for _, event := range s.values {
		if event.Sequence() > after && event.Sequence() <= through && len(result) < limit {
			result = append(result, event)
		}
	}
	return result, nil
}

type channelSubscriber struct {
	values chan events.Event
	errors chan error
}

func newChannelSubscriber() *channelSubscriber {
	return &channelSubscriber{values: make(chan events.Event, 32), errors: make(chan error, 1)}
}

func (s *channelSubscriber) Subscribe(context.Context, domain.TenantID, domain.RunID, uint64) (<-chan events.Event, <-chan error) {
	return s.values, s.errors
}

func TestHandlerReplaysAfterLastEventIDAndClosesOnTerminal(t *testing.T) {
	values := []events.Event{testEvent(t, 1, events.RunStarted), testEvent(t, 2, events.AssistantDelta), testEvent(t, 3, events.RunCompleted)}
	source := &memorySource{values: values}
	handler := testHandler(t, source, newChannelSubscriber())
	request := authenticatedRequest(t, http.MethodGet, "/v1/runs/run_0001/events", "ten_0001")
	request.Header.Set("Last-Event-ID", "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d content-type=%q", response.Code, response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	if strings.Contains(body, "id: 1\n") || !strings.Contains(body, "id: 2\n") || !strings.Contains(body, "id: 3\n") || strings.Count(body, "id: 2\n") != 1 {
		t.Fatalf("unexpected replay body: %s", body)
	}
}

func TestHandlerDeduplicatesLiveOverlap(t *testing.T) {
	initial := testEvent(t, 1, events.RunStarted)
	source := &memorySource{values: []events.Event{initial}}
	subscriber := newChannelSubscriber()
	server := httptest.NewServer(testHandler(t, source, subscriber))
	defer server.Close()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/runs/run_0001/events", nil)
	request.Header.Set("Authorization", "Bearer test")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	subscriber.values <- initial
	subscriber.values <- testEvent(t, 2, events.AssistantDelta)
	subscriber.values <- testEvent(t, 3, events.RunCompleted)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "id: 1\n") != 1 || strings.Count(string(body), "id: 2\n") != 1 || strings.Count(string(body), "id: 3\n") != 1 {
		t.Fatalf("duplicate or missing events: %s", body)
	}
}

func TestHandlerHeartbeatAndRedisFailureFallback(t *testing.T) {
	source := &memorySource{}
	subscriber := newChannelSubscriber()
	configuration := DefaultConfig()
	configuration.HeartbeatInterval = 5 * time.Millisecond
	configuration.PollInterval = 5 * time.Millisecond
	handler, _ := NewHandler(source, subscriber, configuration)
	server := httptest.NewServer(authenticatedRoutes(t, handler.Routes(), "ten_0001"))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/runs/run_0001/events", nil)
	request.Header.Set("Authorization", "Bearer test")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	subscriber.errors <- errors.New("Redis unavailable")
	reader := bufio.NewReader(response.Body)
	found := false
	for index := 0; index < 8; index++ {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.HasPrefix(line, ": heartbeat ") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("heartbeat was not flushed after Redis failure")
	}
}

func TestHandlerRejectsExpiredMalformedAndCrossTenantCursors(t *testing.T) {
	subscriber := newChannelSubscriber()
	tests := []string{"", "01", "+1", "-1", " 1", "1 ", "9223372036854775808", "1,2"}
	for _, value := range tests {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			source := &memorySource{}
			request := authenticatedRequest(t, http.MethodGet, "/v1/runs/run_0001/events", "ten_0001")
			request.Header["Last-Event-Id"] = []string{value}
			response := httptest.NewRecorder()
			testHandler(t, source, subscriber).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_EVENT_CURSOR") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	expired := &memorySource{values: []events.Event{testEvent(t, 1, events.RunStarted), testEvent(t, 2, events.AssistantDelta), testEvent(t, 3, events.AssistantDelta)}, retained: 3}
	request := authenticatedRequest(t, http.MethodGet, "/v1/runs/run_0001/events", "ten_0001")
	request.Header.Set("Last-Event-ID", "1")
	response := httptest.NewRecorder()
	testHandler(t, expired, subscriber).ServeHTTP(response, request)
	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), "EVENT_CURSOR_EXPIRED") {
		t.Fatalf("expired status=%d body=%s", response.Code, response.Body.String())
	}

	foreign := &memorySource{err: events.ErrNotFound}
	absent := &memorySource{err: events.ErrNotFound}
	foreignResponse := callSSE(t, rawHandler(t, foreign, subscriber), "ten_0002")
	absentResponse := callSSE(t, rawHandler(t, absent, subscriber), "ten_0001")
	if foreign.tenant != "ten_0002" || foreignResponse.Code != http.StatusNotFound || foreignResponse.Body.String() != absentResponse.Body.String() {
		t.Fatalf("tenant=%s foreign=%d/%s absent=%d/%s", foreign.tenant, foreignResponse.Code, foreignResponse.Body.String(), absentResponse.Code, absentResponse.Body.String())
	}
}

func TestRelayLiveDisconnectsSlowConsumer(t *testing.T) {
	input := make(chan events.Event, 3)
	output := make(chan events.Event, 1)
	slow := make(chan struct{}, 1)
	input <- testEvent(t, 1, events.RunStarted)
	input <- testEvent(t, 2, events.AssistantDelta)
	input <- testEvent(t, 3, events.RunCompleted)
	go relayLive(context.Background(), input, output, slow)
	select {
	case <-slow:
	case <-time.After(time.Second):
		t.Fatal("slow consumer was not disconnected")
	}
}

func TestHandlerDisconnectsBlockedSlowClient(t *testing.T) {
	source := &memorySource{}
	subscriber := newChannelSubscriber()
	configuration := DefaultConfig()
	configuration.QueueCapacity = 1
	configuration.HeartbeatInterval = time.Hour
	configuration.PollInterval = time.Hour
	handler, err := NewHandler(source, subscriber, configuration)
	if err != nil {
		t.Fatal(err)
	}
	serverHandler := authenticatedRoutes(t, handler.Routes(), "ten_0001")
	response := newBlockingResponseWriter()
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/run_0001/events", nil)
	request.Header.Set("Authorization", "Bearer test")
	finished := make(chan struct{})
	go func() {
		serverHandler.ServeHTTP(response, request)
		close(finished)
	}()
	subscriber.values <- testEvent(t, 1, events.RunStarted)
	select {
	case <-response.blocked:
	case <-time.After(time.Second):
		t.Fatal("writer did not block on first durable event")
	}
	subscriber.values <- testEvent(t, 2, events.AssistantDelta)
	subscriber.values <- testEvent(t, 3, events.RunCompleted)
	time.Sleep(10 * time.Millisecond)
	close(response.release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("slow SSE connection was not disconnected")
	}
}

type blockingResponseWriter struct {
	header  http.Header
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingResponseWriter() *blockingResponseWriter {
	return &blockingResponseWriter{header: make(http.Header), blocked: make(chan struct{}), release: make(chan struct{})}
}

func (w *blockingResponseWriter) Header() http.Header { return w.header }
func (w *blockingResponseWriter) WriteHeader(int)     {}
func (w *blockingResponseWriter) Flush()              {}
func (w *blockingResponseWriter) Write(value []byte) (int, error) {
	if strings.Contains(string(value), "id: 1\n") {
		w.once.Do(func() {
			close(w.blocked)
			<-w.release
		})
	}
	return len(value), nil
}

func testHandler(t *testing.T, source Source, subscriber Subscriber) http.Handler {
	t.Helper()
	return authenticatedRoutes(t, rawHandler(t, source, subscriber), "ten_0001")
}

func rawHandler(t *testing.T, source Source, subscriber Subscriber) http.Handler {
	t.Helper()
	handler, err := NewHandler(source, subscriber, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return handler.Routes()
}

func callSSE(t *testing.T, handler http.Handler, tenant domain.TenantID) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	authenticatedRoutes(t, handler, tenant).ServeHTTP(response, authenticatedRequest(t, http.MethodGet, "/v1/runs/run_0001/events", tenant))
	return response
}

func authenticatedRequest(t *testing.T, method, target string, tenant domain.TenantID) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("Authorization", "Bearer test")
	return request
}

func authenticatedRoutes(t *testing.T, next http.Handler, tenant domain.TenantID) http.Handler {
	t.Helper()
	principal, _ := auth.NewPrincipal(tenant, "subject_0001", []string{"sessions:read"})
	authenticator, _ := auth.NewAuthenticator(auth.VerifierFunc(func(context.Context, string) (auth.Principal, error) { return principal, nil }), nil)
	layer, _ := auth.Middleware(authenticator, nil)
	return layer(next)
}

func testEvent(t *testing.T, sequence uint64, eventType events.Type) events.Event {
	t.Helper()
	var payload any
	switch eventType {
	case events.RunStarted:
		payload = events.EmptyPayload{}
	case events.AssistantDelta:
		payload = events.AssistantDeltaPayload{Text: "delta"}
	case events.RunCompleted:
		payload = events.RunCompletedPayload{MessageID: "msg_0001"}
	default:
		t.Fatalf("unsupported test event type %s", eventType)
	}
	event, err := events.New("ten_0001", "trn_0001", "run_0001", sequence, eventType, payload, time.Date(2026, 8, 23, 0, 0, int(sequence), 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return event
}
