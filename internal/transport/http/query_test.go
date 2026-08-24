package httptransport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/turn"
)

type httpQueryStore struct {
	mu       sync.Mutex
	tenant   domain.TenantID
	logical  *domain.Turn
	run      *domain.Run
	messages []*domain.Message
	notFound bool
}

func (s *httpQueryStore) GetTurn(_ context.Context, tenant domain.TenantID, _ domain.TurnID) (*domain.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenant = tenant
	if s.notFound {
		return nil, turn.ErrQueryNotFound
	}
	return s.logical, nil
}

func (s *httpQueryStore) GetRun(_ context.Context, tenant domain.TenantID, _ domain.RunID) (*domain.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenant = tenant
	if s.notFound {
		return nil, turn.ErrQueryNotFound
	}
	return s.run, nil
}

func (s *httpQueryStore) ListMessages(_ context.Context, tenant domain.TenantID, _ domain.SessionID, after *turn.MessageCursor, limit int) (turn.MessageResultPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenant = tenant
	if s.notFound {
		return turn.MessageResultPage{}, turn.ErrQueryNotFound
	}
	start := 0
	if after != nil {
		for index, message := range s.messages {
			if message.CreatedAt().After(after.CreatedAt) || message.CreatedAt().Equal(after.CreatedAt) && message.ID().String() > after.ID.String() {
				start = index
				break
			}
			start = len(s.messages)
		}
	}
	end := min(start+limit, len(s.messages))
	page := turn.MessageResultPage{Items: s.messages[start:end]}
	if end < len(s.messages) {
		last := s.messages[end-1]
		page.NextCursor = &turn.MessageCursor{CreatedAt: last.CreatedAt(), ID: last.ID()}
	}
	return page, nil
}

func TestQueryHTTPResourcesAndStableHistory(t *testing.T) {
	t.Parallel()
	store := queryHTTPFixture(t)
	codec, _ := turn.NewCursorCodec([]byte("0123456789abcdef0123456789abcdef"))
	service, _ := turn.NewQueryService(store, codec)
	handler, _ := NewQueryHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:read"})
	call := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		request.Header.Set("X-Tenant-ID", "ten_evil")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	turnResponse := call("/v1/turns/trn_0001")
	if turnResponse.Code != 200 || !strings.Contains(turnResponse.Body.String(), `"selected_run_id":"run_0001"`) {
		t.Fatalf("Turn=%d %s", turnResponse.Code, turnResponse.Body.String())
	}
	runResponse := call("/v1/runs/run_0001")
	if runResponse.Code != 200 || !strings.Contains(runResponse.Body.String(), `"terminal":true`) {
		t.Fatalf("Run=%d %s", runResponse.Code, runResponse.Body.String())
	}
	first := call("/v1/sessions/ses_0001/messages?page_size=1")
	var firstPage turn.MessagePageView
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &firstPage) != nil || len(firstPage.Items) != 1 || firstPage.NextCursor == nil {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	second := call("/v1/sessions/ses_0001/messages?page_size=1&cursor=" + *firstPage.NextCursor)
	var secondPage turn.MessagePageView
	if second.Code != 200 || json.Unmarshal(second.Body.Bytes(), &secondPage) != nil || len(secondPage.Items) != 1 || secondPage.Items[0].ID == firstPage.Items[0].ID || secondPage.NextCursor != nil {
		t.Fatalf("second=%d %s", second.Code, second.Body.String())
	}
	if store.tenant != "ten_0001" || first.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("tenant=%s headers=%v", store.tenant, first.Header())
	}
}

func TestQueryHTTPValidationPermissionAndNonEnumeration(t *testing.T) {
	t.Parallel()
	store := queryHTTPFixture(t)
	codec, _ := turn.NewCursorCodec([]byte("0123456789abcdef0123456789abcdef"))
	service, _ := turn.NewQueryService(store, codec)
	handler, _ := NewQueryHandler(service)
	server := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:read"})
	for _, path := range []string{
		"/v1/turns/trn_0001?extra=x",
		"/v1/runs/run_0001?x=1",
		"/v1/sessions/ses_0001/messages?page_size=0",
		"/v1/sessions/ses_0001/messages?page_size=1&page_size=2",
		"/v1/sessions/ses_0001/messages?cursor=tampered",
		"/v1/sessions/ses_0001/messages?unknown=x",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s response=%d %s", path, response.Code, response.Body.String())
		}
	}
	store.notFound = true
	call := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	foreign, absent := call("/v1/turns/trn_0001"), call("/v1/turns/trn_9999")
	if foreign.Code != 404 || absent.Code != 404 || foreign.Body.String() != absent.Body.String() {
		t.Fatalf("foreign=%d %q absent=%d %q", foreign.Code, foreign.Body.String(), absent.Code, absent.Body.String())
	}
	noPermission := authenticatedSessionRoutes(t, handler.Routes(), "ten_0001", []string{"sessions:write"})
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/run_0001", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	noPermission.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatalf("permission response=%d", response.Code)
	}
}

func queryHTTPFixture(t *testing.T) *httpQueryStore {
	t.Helper()
	at := time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC)
	logical, err := domain.RestoreTurn(domain.TurnSnapshot{ID: "trn_0001", TenantID: "ten_0001", SessionID: "ses_0001",
		Sequence: 1, BaseContextVersion: 0, UserMessageID: "msg_0001", Status: domain.TurnStatusCompleted,
		RunIDs: []domain.RunID{"run_0001"}, SelectedRunID: "run_0001", CreatedAt: at, UpdatedAt: at.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	run, err := domain.RestoreRun(domain.RunSnapshot{ID: "run_0001", TenantID: "ten_0001", SessionID: "ses_0001", TurnID: "trn_0001",
		RunNo: 1, Status: domain.RunStatusSelected, OutputMessageID: "msg_0002", CreatedAt: at, UpdatedAt: at.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	user, _ := domain.NewUserMessage("msg_0001", "ten_0001", "ses_0001", "trn_0001", "question", at)
	assistant, _ := domain.NewAssistantCandidate("msg_0002", "ten_0001", "ses_0001", "trn_0001", "run_0001", "answer", at)
	_ = assistant.PromoteToConversation()
	return &httpQueryStore{logical: logical, run: run, messages: []*domain.Message{user, assistant}}
}
