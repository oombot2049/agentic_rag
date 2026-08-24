package httptransport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	sessioncontrol "github.com/agenticrag/agenticrag/internal/session"
	"github.com/agenticrag/agenticrag/internal/turn"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type turnHandlerStore struct {
	mu      sync.Mutex
	command turn.CreatePersistence
	stored  *idempotency.Response
	err     error
}

func TestTurnHTTPPersistsCurrentServerSpanInsteadOfRemoteParent(t *testing.T) {
	store := &turnHandlerStore{}
	service, _ := turn.NewService(store, turn.IDGeneratorFunc(func() (turn.IDs, error) {
		return turn.IDs{TurnID: "trn_0001", MessageID: "msg_0001", RunID: "run_0001", EventID: "evt_0001"}, nil
	}))
	handler, _ := NewTurnHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:write"})
	traceID, _ := oteltrace.TraceIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	spanID, _ := oteltrace.SpanIDFromHex("bbbbbbbbbbbbbbbb")
	spanContext := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: oteltrace.FlagsSampled})
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/ses_0001/turns", strings.NewReader(`{"expected_context_version":0,"message":{"content":"x"}}`))
	request = request.WithContext(oteltrace.ContextWithSpanContext(request.Context(), spanContext))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "idem-key-00000001")
	request.Header.Set("Traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || store.command.Trace.TraceParent != "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01" {
		t.Fatalf("status=%d persisted trace=%+v", response.Code, store.command.Trace)
	}
}

func (s *turnHandlerStore) Create(_ context.Context, command turn.CreatePersistence) (turn.CreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return turn.CreateResult{}, s.err
	}
	if s.stored != nil {
		if s.command.Fingerprint != command.Fingerprint {
			return turn.CreateResult{Decision: idempotency.DecisionKeyReused}, nil
		}
		copy := *s.stored
		return turn.CreateResult{Decision: idempotency.DecisionReplaySucceeded, Response: &copy}, nil
	}
	s.command = command
	view := turn.CreateResponse{SessionID: command.SessionID.String(), TurnID: command.IDs.TurnID.String(),
		RunID: command.IDs.RunID.String(), UserMessageID: command.IDs.MessageID.String(), TurnSequence: 7,
		BaseContextVersion: command.ExpectedContextVersion, Status: "QUEUED",
		EventsURL: "/v1/runs/" + command.IDs.RunID.String() + "/events",
		OutputURL: "/v1/runs/" + command.IDs.RunID.String() + "/output"}
	body, _ := json.Marshal(view)
	s.stored = &idempotency.Response{StatusCode: 202, Body: body, Location: "/v1/runs/" + command.IDs.RunID.String(),
		ResourceType: "run", ResourceID: command.IDs.RunID.String()}
	return turn.CreateResult{Decision: idempotency.DecisionAcquired, Response: s.stored}, nil
}

func TestTurnHTTPCreateReplayAndTrustedTenant(t *testing.T) {
	t.Parallel()
	store := &turnHandlerStore{}
	service, _ := turn.NewService(store, turn.IDGeneratorFunc(func() (turn.IDs, error) {
		return turn.IDs{TurnID: "trn_0001", MessageID: "msg_0001", RunID: "run_0001", EventID: "evt_0001"}, nil
	}))
	handler, _ := NewTurnHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:write"})
	call := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/ses_0001/turns", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer valid-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "idem-key-00000001")
		request.Header.Set("Traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
		request.Header.Set("Tracestate", "vendor=value")
		request.Header.Set("X-Tenant-ID", "ten_evil")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	first := call(`{"expected_context_version":3,"message":{"content":"question"}}`)
	replay := call(`{"expected_context_version":3,"message":{"content":"question"},"execution":{"max_steps":12}}`)
	for _, response := range []*httptest.ResponseRecorder{first, replay} {
		if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/v1/runs/run_0001" ||
			response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
		}
		var body turn.CreateResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.TurnSequence != 7 || body.BaseContextVersion != 3 || body.Status != "QUEUED" {
			t.Fatalf("body=%s err=%v", response.Body.String(), err)
		}
	}
	if first.Body.String() != replay.Body.String() || store.command.TenantID != "ten_0001" || store.command.MaxSteps != 12 ||
		store.command.Trace.TraceParent == "" || store.command.Trace.TraceState != "vendor=value" {
		t.Fatalf("replay or tenant/default mismatch command=%+v", store.command)
	}
}

func TestTurnHTTPRejectsMalformedOrAmbiguousTraceContext(t *testing.T) {
	store := &turnHandlerStore{}
	service, _ := turn.NewService(store, turn.IDGeneratorFunc(func() (turn.IDs, error) {
		return turn.IDs{TurnID: "trn_0001", MessageID: "msg_0001", RunID: "run_0001", EventID: "evt_0001"}, nil
	}))
	handler, _ := NewTurnHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:write"})
	for _, headers := range []http.Header{
		{"Traceparent": []string{"invalid"}},
		{"Tracestate": []string{"vendor=value"}},
		{"Traceparent": []string{"00-11111111111111111111111111111111-2222222222222222-01", "00-33333333333333333333333333333333-4444444444444444-01"}},
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/ses_0001/turns", strings.NewReader(`{"expected_context_version":0,"message":{"content":"x"}}`))
		request.Header.Set("Authorization", "Bearer valid-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "idem-key-00000001")
		for name, values := range headers {
			request.Header[name] = values
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_ARGUMENT") {
			t.Fatalf("headers=%v status=%d body=%s", headers, response.Code, response.Body.String())
		}
	}
}

func TestTurnHTTPValidationPermissionAndConflictCodes(t *testing.T) {
	t.Parallel()
	store := &turnHandlerStore{}
	service, _ := turn.NewService(store, turn.IDGeneratorFunc(func() (turn.IDs, error) {
		return turn.IDs{TurnID: "trn_0001", MessageID: "msg_0001", RunID: "run_0001", EventID: "evt_0001"}, nil
	}))
	handler, _ := NewTurnHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:write"})
	cases := []struct {
		name, contentType, key, body string
	}{
		{"content type", "text/plain", "idem-key-00000001", `{}`},
		{"key", "application/json", "", `{}`},
		{"null", "application/json", "idem-key-00000001", `null`},
		{"null execution", "application/json", "idem-key-00000001", `{"expected_context_version":0,"message":{"content":"x"},"execution":null}`},
		{"unknown execution", "application/json", "idem-key-00000001", `{"expected_context_version":0,"message":{"content":"x"},"execution":{"budget":2}}`},
		{"missing version", "application/json", "idem-key-00000001", `{"message":{"content":"x"}}`},
		{"missing message", "application/json", "idem-key-00000001", `{"expected_context_version":0}`},
		{"unknown", "application/json", "idem-key-00000001", `{"expected_context_version":0,"message":{"content":"x"},"branch":"bad"}`},
		{"trailing", "application/json", "idem-key-00000001", `{"expected_context_version":0,"message":{"content":"x"}} {}`},
		{"max steps", "application/json", "idem-key-00000001", `{"expected_context_version":0,"message":{"content":"x"},"execution":{"max_steps":21}}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/sessions/ses_0001/turns", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer valid-token")
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || !json.Valid(response.Body.Bytes()) {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
		})
	}

	readOnly := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:read"})
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions/ses_0001/turns", strings.NewReader(`{"expected_context_version":0,"message":{"content":"x"}}`))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	readOnly.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("permission response=%d", response.Code)
	}

	for _, conflict := range []struct {
		cause  error
		code   apperror.Code
		status int
	}{
		{sessioncontrol.ErrNotFound, apperror.CodeSessionNotFound, 404},
		{sessioncontrol.ErrVersionConflict, apperror.CodeSessionVersionConflict, 409},
		{sessioncontrol.ErrBusy, apperror.CodeSessionBusy, 409},
	} {
		failureStore := &turnHandlerStore{err: conflict.cause}
		failureService, _ := turn.NewService(failureStore, turn.IDGeneratorFunc(func() (turn.IDs, error) {
			return turn.IDs{TurnID: domain.TurnID("trn_0001"), MessageID: "msg_0001", RunID: "run_0001", EventID: "evt_0001"}, nil
		}))
		failureHandler, _ := NewTurnHandler(failureService)
		failureServer := authenticatedSessionRoutes(t, failureHandler.Routes(), "ten_0001", []string{"sessions:write"})
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions/ses_0001/turns", strings.NewReader(`{"expected_context_version":0,"message":{"content":"x"}}`))
		request.Header.Set("Authorization", "Bearer valid-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "idem-key-00000001")
		response := httptest.NewRecorder()
		failureServer.ServeHTTP(response, request)
		if response.Code != conflict.status || !strings.Contains(response.Body.String(), string(conflict.code)) {
			t.Errorf("cause=%v response=%d %s", conflict.cause, response.Code, response.Body.String())
		}
	}
}
