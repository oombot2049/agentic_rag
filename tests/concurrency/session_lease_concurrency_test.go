//go:build concurrency

package concurrency_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/domain"
	platformpostgres "github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/repository"
	"github.com/agenticrag/agenticrag/internal/runlease"
	"github.com/agenticrag/agenticrag/internal/session"
	httptransport "github.com/agenticrag/agenticrag/internal/transport/http"
	"github.com/agenticrag/agenticrag/internal/turn"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	concurrencyPostgresImage = "postgres:18.6-alpine"
	concurrencyToken         = "concurrency-bearer-token"
	apiReplicaCount          = 4
	sameSessionClients       = 64
	parallelSessionCount     = 32
)

type sessionView struct {
	ID             string `json:"id"`
	ContextVersion uint64 `json:"context_version"`
	RowVersion     uint64 `json:"row_version"`
}

type turnView struct {
	TurnID string `json:"turn_id"`
	RunID  string `json:"run_id"`
}

type requestResult struct {
	status int
	code   string
	turn   turnView
	err    error
}

func TestSessionContentionParallelismAndLeaseTakeover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	harness := newConcurrencyHarness(t, ctx)

	contended := harness.createSession(t, 0)
	winner := harness.competeForSession(t, contended)
	harness.assertSingleReservation(t, ctx, contended.ID, winner)

	harness.submitDifferentSessions(t, ctx)
	harness.assertLeaseTakeoverAndSingleFinal(t, ctx, contended, winner)
}

type concurrencyHarness struct {
	pool      *platformpostgres.Pool
	replicas  []*httptest.Server
	client    *http.Client
	tenantID  domain.TenantID
	sessionID *sequenceSessionIDs
	turnIDs   *sequenceTurnIDs
}

func newConcurrencyHarness(t *testing.T, ctx context.Context) *concurrencyHarness {
	t.Helper()
	container, err := tcpostgres.Run(ctx, concurrencyPostgresImage, tcpostgres.WithDatabase("agenticrag"),
		tcpostgres.WithUsername("agenticrag"), tcpostgres.WithPassword("concurrency-secret"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		if os.Getenv("AGENTICRAG_REQUIRE_DOCKER") == "1" {
			t.Fatal(err)
		}
		t.Skipf("Docker unavailable: %v", err)
	}
	registerContainer(t, container)
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadSources(config.MapSource{"AGENTICRAG_POSTGRES_DSN": dsn})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := platformpostgres.Open(ctx, cfg.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	migrator, err := platformpostgres.NewMigrator(pool, coremigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}

	tenantID := domain.TenantID("ten_concurrency")
	principal, err := auth.NewPrincipal(tenantID, "concurrency-subject", []string{"sessions:read", "sessions:write"})
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.NewAuthenticator(auth.VerifierFunc(func(_ context.Context, token string) (auth.Principal, error) {
		if token != concurrencyToken {
			return auth.Principal{}, auth.ErrInvalidCredential
		}
		return principal, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := auth.Middleware(authenticator, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessionIDs, turnIDs := &sequenceSessionIDs{}, &sequenceTurnIDs{}
	withinTx := func(ctx context.Context, options *sql.TxOptions, work func(context.Context, repository.DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	}
	replicas := make([]*httptest.Server, 0, apiReplicaCount)
	for range apiReplicaCount {
		sessionStore, err := repository.NewSessionAPIStore(pool, withinTx)
		if err != nil {
			t.Fatal(err)
		}
		sessionService, err := session.NewService(sessionStore, sessionIDs)
		if err != nil {
			t.Fatal(err)
		}
		turnStore, err := repository.NewTurnAPIStore(withinTx)
		if err != nil {
			t.Fatal(err)
		}
		turnService, err := turn.NewService(turnStore, turnIDs)
		if err != nil {
			t.Fatal(err)
		}
		sessionHandler, _ := httptransport.NewSessionHandler(sessionService)
		turnHandler, _ := httptransport.NewTurnHandler(turnService)
		mux := http.NewServeMux()
		mux.Handle("POST /v1/sessions", sessionHandler.Routes())
		mux.Handle("POST /v1/sessions/{session_id}/turns", turnHandler.Routes())
		server := httptest.NewServer(middleware(mux))
		replicas = append(replicas, server)
		t.Cleanup(server.Close)
	}
	return &concurrencyHarness{pool: pool, replicas: replicas, client: replicas[0].Client(), tenantID: tenantID,
		sessionID: sessionIDs, turnIDs: turnIDs}
}

func (h *concurrencyHarness) createSession(t *testing.T, replica int) sessionView {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.replicas[replica%len(h.replicas)].URL+"/v1/sessions", strings.NewReader(`{"metadata":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+concurrencyToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", fmt.Sprintf("session-concurrency-%08d", h.sessionID.Peek()+1))
	response, err := h.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("create Session status=%d body=%s", response.StatusCode, body)
	}
	var result sessionView
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (h *concurrencyHarness) competeForSession(t *testing.T, target sessionView) turnView {
	t.Helper()
	start := make(chan struct{})
	results := make(chan requestResult, sameSessionClients)
	var group sync.WaitGroup
	for index := range sameSessionClients {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			body := fmt.Sprintf(`{"expected_context_version":0,"message":{"content":"contender %d"}}`, index)
			results <- h.createTurn(index, target.ID, fmt.Sprintf("turn-contention-%08d", index), body)
		}(index)
	}
	close(start)
	group.Wait()
	close(results)

	accepted := 0
	winner := turnView{}
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		switch result.status {
		case http.StatusAccepted:
			accepted++
			winner = result.turn
		case http.StatusConflict:
			if result.code != "SESSION_BUSY" {
				t.Errorf("loser error code = %q", result.code)
			}
		default:
			t.Errorf("contention status=%d code=%q", result.status, result.code)
		}
	}
	if accepted != 1 {
		t.Fatalf("same-Session accepted requests = %d, want 1", accepted)
	}
	return winner
}

func (h *concurrencyHarness) createTurn(replica int, sessionID, key, body string) requestResult {
	request, err := http.NewRequest(http.MethodPost, h.replicas[replica%len(h.replicas)].URL+"/v1/sessions/"+sessionID+"/turns", strings.NewReader(body))
	if err != nil {
		return requestResult{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+concurrencyToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response, err := h.client.Do(request)
	if err != nil {
		return requestResult{err: err}
	}
	defer response.Body.Close()
	result := requestResult{status: response.StatusCode}
	if response.StatusCode == http.StatusAccepted {
		result.err = json.NewDecoder(response.Body).Decode(&result.turn)
		return result
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	result.err = json.NewDecoder(response.Body).Decode(&failure)
	result.code = failure.Error.Code
	return result
}

func (h *concurrencyHarness) assertSingleReservation(t *testing.T, ctx context.Context, sessionID string, winner turnView) {
	t.Helper()
	var turns, runs, messages, outboxRows int
	var activeRun sql.NullString
	err := h.pool.QueryRowContext(ctx, `SELECT
	 (SELECT count(*) FROM turns WHERE tenant_id=$1 AND session_id=$2),
	 (SELECT count(*) FROM runs WHERE tenant_id=$1 AND session_id=$2),
	 (SELECT count(*) FROM messages WHERE tenant_id=$1 AND session_id=$2 AND role='USER'),
	 (SELECT count(*) FROM outbox_events WHERE tenant_id=$1 AND aggregate_id=$3 AND event_type='RunQueued.v1'),
	 (SELECT active_run_id FROM sessions WHERE tenant_id=$1 AND id=$2)`,
		h.tenantID.String(), sessionID, winner.RunID).Scan(&turns, &runs, &messages, &outboxRows, &activeRun)
	if err != nil || turns != 1 || runs != 1 || messages != 1 || outboxRows != 1 || !activeRun.Valid || activeRun.String != winner.RunID {
		t.Fatalf("reservation turns=%d runs=%d messages=%d outbox=%d active=%v error=%v", turns, runs, messages, outboxRows, activeRun, err)
	}
}

func (h *concurrencyHarness) submitDifferentSessions(t *testing.T, ctx context.Context) {
	t.Helper()
	sessions := make([]sessionView, parallelSessionCount)
	for index := range sessions {
		sessions[index] = h.createSession(t, index)
	}
	start := make(chan struct{})
	results := make(chan requestResult, len(sessions))
	var group sync.WaitGroup
	for index, value := range sessions {
		group.Add(1)
		go func(index int, value sessionView) {
			defer group.Done()
			<-start
			body := fmt.Sprintf(`{"expected_context_version":0,"message":{"content":"parallel Session %d"}}`, index)
			results <- h.createTurn(index, value.ID, fmt.Sprintf("turn-parallel-%08d", index), body)
		}(index, value)
	}
	close(start)
	group.Wait()
	close(results)
	runIDs := make(map[string]struct{}, len(sessions))
	for result := range results {
		if result.err != nil || result.status != http.StatusAccepted {
			t.Fatalf("different-Session result=%+v", result)
		}
		runIDs[result.turn.RunID] = struct{}{}
	}
	if len(runIDs) != len(sessions) {
		t.Fatalf("distinct parallel Run IDs = %d, want %d", len(runIDs), len(sessions))
	}
	var active, turns, runs int
	ids := make([]any, 0, len(sessions)+1)
	ids = append(ids, h.tenantID.String())
	placeholders := make([]string, len(sessions))
	for index, value := range sessions {
		ids = append(ids, value.ID)
		placeholders[index] = "$" + strconv.Itoa(index+2)
	}
	query := `SELECT count(*) FILTER (WHERE active_run_id IS NOT NULL),
	 (SELECT count(*) FROM turns WHERE tenant_id=$1 AND session_id IN (` + strings.Join(placeholders, ",") + `)),
	 (SELECT count(*) FROM runs WHERE tenant_id=$1 AND session_id IN (` + strings.Join(placeholders, ",") + `))
	 FROM sessions WHERE tenant_id=$1 AND id IN (` + strings.Join(placeholders, ",") + `)`
	if err := h.pool.QueryRowContext(ctx, query, ids...).Scan(&active, &turns, &runs); err != nil || active != len(sessions) || turns != len(sessions) || runs != len(sessions) {
		t.Fatalf("parallel active=%d turns=%d runs=%d error=%v", active, turns, runs, err)
	}
}

func (h *concurrencyHarness) assertLeaseTakeoverAndSingleFinal(t *testing.T, ctx context.Context, sessionState sessionView, winner turnView) {
	t.Helper()
	execution, err := repository.NewExecution(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	managerA, err := runlease.New(execution, runlease.Config{Owner: "worker-a", LeaseDuration: 5 * time.Second,
		RenewInterval: time.Second, Clock: logicalClock{now: t0}})
	if err != nil {
		t.Fatal(err)
	}
	managerB, err := runlease.New(execution, runlease.Config{Owner: "worker-b", LeaseDuration: 5 * time.Second,
		RenewInterval: time.Second, Clock: logicalClock{now: t0.Add(6 * time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := domain.ParseRunID(winner.RunID)
	first, err := managerA.Acquire(ctx, runlease.AcquireRequest{TenantID: h.tenantID, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := managerB.Acquire(ctx, runlease.AcquireRequest{TenantID: h.tenantID, RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	firstAuthority, secondAuthority := first.Handle.Snapshot(), second.Handle.Snapshot()
	if secondAuthority.Token <= firstAuthority.Token || secondAuthority.ExecutionVersion <= firstAuthority.ExecutionVersion {
		t.Fatalf("takeover authority first=%+v second=%+v", firstAuthority, secondAuthority)
	}

	var contextVersion, rowVersion uint64
	if err := h.pool.QueryRowContext(ctx, `SELECT context_version,row_version FROM sessions WHERE tenant_id=$1 AND id=$2`,
		h.tenantID.String(), sessionState.ID).Scan(&contextVersion, &rowVersion); err != nil {
		t.Fatal(err)
	}
	sessionID, _ := domain.ParseSessionID(sessionState.ID)
	turnID, _ := domain.ParseTurnID(winner.TurnID)
	messageID, _ := domain.ParseMessageID("msg_confinal01")
	finalStore, err := repository.NewFinalCommitStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, repository.DBTX) error) error {
		return h.pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := turn.NewFinalCommitService(finalStore)
	if err != nil {
		t.Fatal(err)
	}
	request := turn.FinalRequest{TenantID: h.tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID,
		MessageID: messageID, Content: "single concurrent final", ExpectedContextVersion: contextVersion,
		ExpectedSessionRowVersion: rowVersion, At: t0.Add(6 * time.Second), EventRetention: time.Hour}
	request.Authority = turn.FinalAuthority{Owner: firstAuthority.Owner, Token: firstAuthority.Token,
		ExecutionVersion: firstAuthority.ExecutionVersion}
	if _, err := service.Commit(ctx, request); !errors.Is(err, turn.ErrFinalFenced) { // ErrFinalFenced is the required stale-writer result.
		t.Fatalf("stale final commit error = %v", err)
	}
	request.Authority = turn.FinalAuthority{Owner: secondAuthority.Owner, Token: secondAuthority.Token,
		ExecutionVersion: secondAuthority.ExecutionVersion}
	committed, err := service.Commit(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Commit(ctx, request)
	if err != nil || !replayed.Replayed || replayed.MessageID != committed.MessageID || replayed.EventSequence != committed.EventSequence {
		t.Fatalf("final replay committed=%+v replayed=%+v error=%v", committed, replayed, err)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer closeCancel()
	_ = first.Handle.Close(closeCtx)
	if err := second.Handle.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	var assistants, completions int
	if err := h.pool.QueryRowContext(ctx, `SELECT
	 (SELECT count(*) FROM messages WHERE tenant_id=$1 AND run_id=$2 AND role='ASSISTANT' AND visibility='CONVERSATION'),
	 (SELECT count(*) FROM run_events WHERE tenant_id=$1 AND run_id=$2 AND event_type='run.completed')`,
		h.tenantID.String(), winner.RunID).Scan(&assistants, &completions); err != nil || assistants != 1 || completions != 1 {
		t.Fatalf("final rows assistants=%d completed_events=%d error=%v", assistants, completions, err)
	}
}

type sequenceSessionIDs struct {
	mu   sync.Mutex
	next int
}

func (s *sequenceSessionIDs) NewSessionID() (domain.SessionID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return domain.ParseSessionID(fmt.Sprintf("ses_con%08d", s.next))
}

func (s *sequenceSessionIDs) Peek() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

type sequenceTurnIDs struct {
	mu   sync.Mutex
	next int
}

func (s *sequenceTurnIDs) NewTurnIDs() (turn.IDs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	suffix := fmt.Sprintf("con%08d", s.next)
	turnID, err := domain.ParseTurnID("trn_" + suffix)
	if err != nil {
		return turn.IDs{}, err
	}
	messageID, err := domain.ParseMessageID("msg_" + suffix)
	if err != nil {
		return turn.IDs{}, err
	}
	runID, err := domain.ParseRunID("run_" + suffix)
	if err != nil {
		return turn.IDs{}, err
	}
	return turn.IDs{TurnID: turnID, MessageID: messageID, RunID: runID, EventID: "evt_" + suffix}, nil
}

type logicalClock struct{ now time.Time }

func (c logicalClock) Now() time.Time { return c.now }
func (logicalClock) NewTicker(time.Duration) runlease.Ticker {
	return quietTicker{channel: make(chan time.Time)}
}

type quietTicker struct{ channel <-chan time.Time }

func (t quietTicker) C() <-chan time.Time { return t.channel }
func (quietTicker) Stop()                 {}

func registerContainer(t *testing.T, container testcontainers.Container) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if logs, err := container.Logs(ctx); err == nil {
				data, _ := io.ReadAll(io.LimitReader(logs, 256<<10))
				_ = logs.Close()
				t.Logf("PostgreSQL logs (bounded):\n%s", data)
			}
		}
		testcontainers.CleanupContainer(t, container)
	})
}
