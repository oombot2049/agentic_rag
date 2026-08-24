package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	"github.com/agenticrag/agenticrag/internal/session"
)

type handlerStore struct {
	mu       sync.Mutex
	records  map[string]session.CreatePersistence
	sessions map[string]*domain.Session
	tenant   domain.TenantID
}

func newHandlerStore() *handlerStore {
	return &handlerStore{records: map[string]session.CreatePersistence{}, sessions: map[string]*domain.Session{}}
}

func (s *handlerStore) Create(_ context.Context, command session.CreatePersistence) (session.CreateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenant = command.Scope.TenantID
	key := command.Scope.TenantID.String() + "/" + command.Scope.AuthenticatedSubjectID + "/" + command.Scope.Key
	if old, ok := s.records[key]; ok {
		if old.Fingerprint != command.Fingerprint {
			return session.CreateResult{Decision: idempotency.DecisionKeyReused}, nil
		}
		response := old.Response
		return session.CreateResult{Decision: idempotency.DecisionReplaySucceeded, Response: &response}, nil
	}
	s.records[key] = command
	s.sessions[command.Scope.TenantID.String()+"/"+command.Session.ID().String()] = command.Session
	return session.CreateResult{Decision: idempotency.DecisionAcquired, Session: command.Session}, nil
}

func (s *handlerStore) Get(_ context.Context, tenant domain.TenantID, id domain.SessionID) (*domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[tenant.String()+"/"+id.String()]
	if !ok {
		return nil, session.ErrNotFound
	}
	return value, nil
}

func TestSessionHTTPCreateReplayGetAndTrustedTenant(t *testing.T) {
	t.Parallel()
	store := newHandlerStore()
	service, _ := session.NewService(store, session.IDGeneratorFunc(func() (domain.SessionID, error) { return "ses_0001", nil }))
	handler, _ := NewSessionHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:read", "sessions:write"})

	create := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{"metadata":{"channel":"web"}}`))
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
		request.Header.Set("Authorization", "Bearer valid-token")
		request.Header.Set("Idempotency-Key", "idem-key-00000001")
		request.Header.Set("X-Tenant-ID", "ten_evil")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	first, replay := create(), create()
	for _, response := range []*httptest.ResponseRecorder{first, replay} {
		if response.Code != http.StatusCreated || response.Header().Get("Location") != "/v1/sessions/ses_0001" || response.Header().Get("ETag") != `"rv-1"` {
			t.Fatalf("create status/headers = %d %v body=%s", response.Code, response.Header(), response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("security headers = %v", response.Header())
		}
		var view session.SessionView
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || view.ID != "ses_0001" || view.ContextVersion != 0 || view.RowVersion != 1 {
			t.Fatalf("create body = %s, err=%v", response.Body.String(), err)
		}
	}
	if first.Body.String() != replay.Body.String() || store.tenant != "ten_0001" || len(store.records) != 1 {
		t.Fatalf("replay/trusted tenant mismatch: tenant=%s records=%d", store.tenant, len(store.records))
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_0001", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("ETag") != `"rv-1"` || response.Body.String() != first.Body.String() {
		t.Fatalf("GET = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestSessionHTTPStrictValidationAndAuthorization(t *testing.T) {
	t.Parallel()
	store := newHandlerStore()
	service, _ := session.NewService(store, session.IDGeneratorFunc(func() (domain.SessionID, error) { return "ses_0001", nil }))
	handler, _ := NewSessionHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:read", "sessions:write"})
	cases := []struct {
		name, contentType, key, body string
		want                         int
	}{
		{"missing content type", "", "idem-key-00000001", `{}`, 400},
		{"wrong content type", "text/plain", "idem-key-00000001", `{}`, 400},
		{"missing body", "application/json", "idem-key-00000001", ``, 400},
		{"null body", "application/json", "idem-key-00000001", `null`, 400},
		{"unknown field", "application/json", "idem-key-00000001", `{"tenant_id":"ten_evil"}`, 400},
		{"trailing JSON", "application/json", "idem-key-00000001", `{} {}`, 400},
		{"short key", "application/json", "short", `{}`, 400},
		{"bad metadata", "application/json", "idem-key-00000001", `{"metadata":{" bad":"x"}}`, 400},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer valid-token")
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			if test.key != "" {
				request.Header.Set("Idempotency-Key", test.key)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.want || !json.Valid(response.Body.Bytes()) {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
		})
	}

	large := bytes.Repeat([]byte("x"), maximumCreateSessionBody+1)
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions", io.MultiReader(strings.NewReader(`{"metadata":{"x":"`), bytes.NewReader(large), strings.NewReader(`"}}`)))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "idem-key-00000001")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized response = %d", response.Code)
	}

	readOnly := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:read"})
	request = httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "idem-key-00000001")
	response = httptest.NewRecorder()
	readOnly.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("permission response = %d %s", response.Code, response.Body.String())
	}
}

func TestSessionHTTPForeignAndAbsentAreIndistinguishable(t *testing.T) {
	t.Parallel()
	store := newHandlerStore()
	foreign, _ := domain.NewSession("ses_0001", "ten_0002", time.Now())
	store.sessions["ten_0002/ses_0001"] = foreign
	service, _ := session.NewService(store, nil)
	handler, _ := NewSessionHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:read"})
	call := func(id string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+id, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	foreignResponse, absentResponse := call("ses_0001"), call("ses_9999")
	if foreignResponse.Code != http.StatusNotFound || absentResponse.Code != http.StatusNotFound || foreignResponse.Body.String() != absentResponse.Body.String() {
		t.Fatalf("foreign=%d %q absent=%d %q", foreignResponse.Code, foreignResponse.Body.String(), absentResponse.Code, absentResponse.Body.String())
	}
}

func authenticatedSessionRoutes(t *testing.T, next http.Handler, tenant domain.TenantID, permissions []string) http.Handler {
	t.Helper()
	principal, err := auth.NewPrincipal(tenant, "subject_0001", permissions)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.NewAuthenticator(auth.VerifierFunc(func(context.Context, string) (auth.Principal, error) {
		return principal, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	layer, err := auth.Middleware(authenticator, nil)
	if err != nil {
		t.Fatal(err)
	}
	return layer(next)
}
