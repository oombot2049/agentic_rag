package httptransport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/idempotency"
	runcontrol "github.com/agenticrag/agenticrag/internal/run"
)

type cancelHTTPStore struct{ command runcontrol.CancelPersistence }

func (s *cancelHTTPStore) Cancel(_ context.Context, command runcontrol.CancelPersistence) (runcontrol.CancelResult, error) {
	s.command = command
	response := runcontrol.CancelResponse{RunID: command.RunID.String(), Status: "CANCEL_REQUESTED", CancellationRequested: true}
	body, _ := json.Marshal(response)
	return runcontrol.CancelResult{Decision: idempotency.DecisionAcquired, Response: &idempotency.Response{
		StatusCode: 202, Body: body, ResourceType: "run", ResourceID: command.RunID.String()}}, nil
}

func TestRunCancelHTTPUsesTrustedTenantAndRequiresIdempotency(t *testing.T) {
	store := &cancelHTTPStore{}
	service, _ := runcontrol.NewService(store, runcontrol.EventIDGeneratorFunc(func() (string, error) { return "evt_cancel0001", nil }))
	handler, _ := NewRunCancelHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:write"})
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run_0001:cancel", nil)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Idempotency-Key", "cancel-key-00000001")
	request.Header.Set("X-Tenant-ID", "ten_evil")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || response.Header().Get("Location") != "/v1/runs/run_0001" ||
		store.command.TenantID != "ten_0001" || !strings.Contains(response.Body.String(), `"status":"CANCEL_REQUESTED"`) {
		t.Fatalf("status=%d headers=%v body=%s command=%+v", response.Code, response.Header(), response.Body.String(), store.command)
	}

	missing := httptest.NewRequest(http.MethodPost, "/v1/runs/run_0001:cancel", nil)
	missing.Header.Set("Authorization", "Bearer token")
	missingResponse := httptest.NewRecorder()
	server.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusBadRequest || !strings.Contains(missingResponse.Body.String(), "INVALID_IDEMPOTENCY_KEY") {
		t.Fatalf("missing key=%d %s", missingResponse.Code, missingResponse.Body.String())
	}
}

func TestRunCancelHTTPRejectsBodyQueryAndPermission(t *testing.T) {
	store := &cancelHTTPStore{}
	service, _ := runcontrol.NewService(store, runcontrol.EventIDGeneratorFunc(func() (string, error) { return "evt_cancel0001", nil }))
	handler, _ := NewRunCancelHandler(service)
	for _, target := range []string{"/v1/runs/run_0001:cancel?force=true", "/v1/runs/run_0001:cancel"} {
		server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:write"})
		body := ""
		if !strings.Contains(target, "?") {
			body = `{}`
		}
		request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		request.Header.Set("Idempotency-Key", "cancel-key-00000001")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_ARGUMENT") {
			t.Fatalf("target=%s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	readOnly := authenticatedSessionRoutes(t, handler.Routes(), domain.TenantID("ten_0001"), []string{"sessions:read"})
	request := httptest.NewRequest(http.MethodPost, "/v1/runs/run_0001:cancel", nil)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Idempotency-Key", "cancel-key-00000001")
	response := httptest.NewRecorder()
	readOnly.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("read-only status=%d body=%s", response.Code, response.Body.String())
	}
}
