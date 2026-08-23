//go:build e2e

package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/agent"
	"github.com/agenticrag/agenticrag/internal/auth"
	"github.com/agenticrag/agenticrag/internal/config"
	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/events"
	"github.com/agenticrag/agenticrag/internal/outbox"
	platformpostgres "github.com/agenticrag/agenticrag/internal/platform/postgres"
	"github.com/agenticrag/agenticrag/internal/queue"
	"github.com/agenticrag/agenticrag/internal/repository"
	"github.com/agenticrag/agenticrag/internal/runlease"
	"github.com/agenticrag/agenticrag/internal/session"
	"github.com/agenticrag/agenticrag/internal/tools"
	"github.com/agenticrag/agenticrag/internal/tools/builtin"
	toolexecutor "github.com/agenticrag/agenticrag/internal/tools/executor"
	httptransport "github.com/agenticrag/agenticrag/internal/transport/http"
	"github.com/agenticrag/agenticrag/internal/transport/sse"
	"github.com/agenticrag/agenticrag/internal/turn"
	"github.com/agenticrag/agenticrag/internal/worker"
	coremigrations "github.com/agenticrag/agenticrag/migrations"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	e2ePostgresImage = "postgres:18.6-alpine"
	e2eToken         = "e2e-bearer-token"
	e2eAnswer        = "Refunds settle in five business days [doc_refund/chk_refund]."
)

type createSessionResponse struct {
	ID             string  `json:"id"`
	ContextVersion uint64  `json:"context_version"`
	RowVersion     uint64  `json:"row_version"`
	ActiveRunID    *string `json:"active_run_id"`
}

type createTurnResponse struct {
	SessionID     string `json:"session_id"`
	TurnID        string `json:"turn_id"`
	RunID         string `json:"run_id"`
	UserMessageID string `json:"user_message_id"`
	EventsURL     string `json:"events_url"`
}

type runResponse struct {
	Status         string  `json:"status"`
	Terminal       bool    `json:"terminal"`
	FinalMessageID *string `json:"final_message_id"`
}

type messageResponse struct {
	Items []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"items"`
}

type publicFailure struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func TestCreateTurnThroughAgentLoopAndResumeSSE(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	harness := newHarness(t, ctx)

	sessionResponse := doJSON[createSessionResponse](t, harness.client, http.MethodPost, harness.url+"/v1/sessions",
		"session-idempotency-0001", `{"metadata":{"suite":"e2e"}}`, http.StatusCreated)
	if sessionResponse.ContextVersion != 0 || sessionResponse.RowVersion != 1 {
		t.Fatalf("new Session versions = context:%d row:%d", sessionResponse.ContextVersion, sessionResponse.RowVersion)
	}

	turnBody := `{"expected_context_version":0,"message":{"content":"When will my refund arrive?"},"execution":{"max_steps":4}}`
	created := doJSON[createTurnResponse](t, harness.client, http.MethodPost,
		harness.url+"/v1/sessions/"+sessionResponse.ID+"/turns", "turn-idempotency-0001", turnBody, http.StatusAccepted)
	replayed := doJSON[createTurnResponse](t, harness.client, http.MethodPost,
		harness.url+"/v1/sessions/"+sessionResponse.ID+"/turns", "turn-idempotency-0001", turnBody, http.StatusAccepted)
	if created != replayed {
		t.Fatalf("idempotent replay changed identity: first=%+v replay=%+v", created, replayed)
	}

	reused := doFailure(t, harness.client, http.MethodPost, harness.url+"/v1/sessions/"+sessionResponse.ID+"/turns",
		"turn-idempotency-0001", `{"expected_context_version":0,"message":{"content":"different"}}`, http.StatusConflict)
	if reused.Error.Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("key reuse code = %q", reused.Error.Code)
	}
	versionConflict := doFailure(t, harness.client, http.MethodPost, harness.url+"/v1/sessions/"+sessionResponse.ID+"/turns",
		"turn-idempotency-0002", `{"expected_context_version":7,"message":{"content":"future version"}}`, http.StatusConflict)
	if versionConflict.Error.Code != "SESSION_VERSION_CONFLICT" {
		t.Fatalf("version conflict code = %q", versionConflict.Error.Code)
	}

	firstStream := openSSE(t, harness.client, harness.url+created.EventsURL, "")
	harness.dispatch(t, ctx)
	first := readSSEEvent(t, firstStream.Body)
	_ = firstStream.Body.Close()
	if first.ID == 0 || first.Type == "" {
		t.Fatalf("first SSE event = %+v", first)
	}

	resumedStream := openSSE(t, harness.client, harness.url+created.EventsURL, strconv.FormatUint(first.ID, 10))
	remaining := readUntilTerminal(t, resumedStream.Body)
	_ = resumedStream.Body.Close()
	assertContiguousResume(t, first, remaining)

	runView := doJSON[runResponse](t, harness.client, http.MethodGet, harness.url+"/v1/runs/"+created.RunID, "", "", http.StatusOK)
	if !runView.Terminal || runView.Status != "SELECTED" || runView.FinalMessageID == nil {
		t.Fatalf("final Run = %+v", runView)
	}
	messages := doJSON[messageResponse](t, harness.client, http.MethodGet,
		harness.url+"/v1/sessions/"+sessionResponse.ID+"/messages", "", "", http.StatusOK)
	if len(messages.Items) != 2 || messages.Items[0].Role != "USER" || messages.Items[1].Role != "ASSISTANT" ||
		messages.Items[1].Content != e2eAnswer || !strings.Contains(messages.Items[1].Content, "doc_refund/chk_refund") {
		t.Fatalf("conversation messages = %+v", messages.Items)
	}
	committed := doJSON[createSessionResponse](t, harness.client, http.MethodGet,
		harness.url+"/v1/sessions/"+sessionResponse.ID, "", "", http.StatusOK)
	if committed.ContextVersion != 1 || committed.ActiveRunID != nil {
		t.Fatalf("committed Session = %+v", committed)
	}

	harness.assertDurableEffects(t, ctx, created)
}

type harness struct {
	pool       *platformpostgres.Pool
	client     *http.Client
	url        string
	tenantID   domain.TenantID
	queue      *queue.PostgresAdapter
	relay      *outbox.Relay
	worker     *worker.Worker
	processErr chan error
	searched   *searchProbe
}

func newHarness(t *testing.T, ctx context.Context) *harness {
	t.Helper()
	container, err := tcpostgres.Run(ctx, e2ePostgresImage, tcpostgres.WithDatabase("agenticrag"),
		tcpostgres.WithUsername("agenticrag"), tcpostgres.WithPassword("e2e-secret"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		if os.Getenv("AGENTICRAG_REQUIRE_DOCKER") == "1" {
			t.Fatal(err)
		}
		t.Skipf("Docker unavailable: %v", err)
	}
	registerPostgres(t, container)
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

	tenantID := domain.TenantID("ten_e2e0001")
	principal, err := auth.NewPrincipal(tenantID, "subject-e2e", []string{"sessions:read", "sessions:write"})
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.NewAuthenticator(auth.VerifierFunc(func(_ context.Context, token string) (auth.Principal, error) {
		if token != e2eToken {
			return auth.Principal{}, auth.ErrInvalidCredential
		}
		return principal, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	authMiddleware, err := auth.Middleware(authenticator, nil)
	if err != nil {
		t.Fatal(err)
	}
	withinRepository := func(ctx context.Context, options *sql.TxOptions, work func(context.Context, repository.DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	}
	sessionStore, err := repository.NewSessionAPIStore(pool, withinRepository)
	if err != nil {
		t.Fatal(err)
	}
	sessionService, err := session.NewService(sessionStore, session.IDGeneratorFunc(func() (domain.SessionID, error) {
		return domain.ParseSessionID("ses_e2e0001")
	}))
	if err != nil {
		t.Fatal(err)
	}
	turnStore, err := repository.NewTurnAPIStore(withinRepository)
	if err != nil {
		t.Fatal(err)
	}
	turnService, err := turn.NewService(turnStore, turn.IDGeneratorFunc(func() (turn.IDs, error) {
		return fixedTurnIDs()
	}))
	if err != nil {
		t.Fatal(err)
	}
	queryStore, err := repository.NewTurnQueryStore(withinRepository)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := turn.NewCursorCodec([]byte("e2e-cursor-secret-at-least-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	queryService, err := turn.NewQueryService(queryStore, cursor)
	if err != nil {
		t.Fatal(err)
	}
	eventRepository, err := events.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	sseHandler, err := sse.NewHandler(eventRepository, pollingSubscriber{}, sse.Config{
		HeartbeatInterval: time.Second, PollInterval: 10 * time.Millisecond, WriteTimeout: time.Second,
		RetryDelay: time.Second, ReplayPageSize: 32, QueueCapacity: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionHandler, _ := httptransport.NewSessionHandler(sessionService)
	turnHandler, _ := httptransport.NewTurnHandler(turnService)
	queryHandler, _ := httptransport.NewQueryHandler(queryService)
	root := http.NewServeMux()
	root.Handle("POST /v1/sessions", sessionHandler.Routes())
	root.Handle("GET /v1/sessions/{session_id}", sessionHandler.Routes())
	root.Handle("POST /v1/sessions/{session_id}/turns", turnHandler.Routes())
	root.Handle("GET /v1/runs/{run_id}/events", sseHandler.Routes())
	root.Handle("GET /v1/turns/{turn_id}", queryHandler.Routes())
	root.Handle("GET /v1/runs/{run_id}", queryHandler.Routes())
	root.Handle("GET /v1/sessions/{session_id}/messages", queryHandler.Routes())
	server := httptest.NewServer(authMiddleware(root))
	t.Cleanup(server.Close)

	withinQueue := func(ctx context.Context, options *sql.TxOptions, work func(context.Context, queue.DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	}
	queueAdapter, err := queue.NewPostgresAdapter(withinQueue, queue.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	queuePublisher, err := queue.NewRunQueuedAdapter(queueAdapter)
	if err != nil {
		t.Fatal(err)
	}
	outboxRepository, err := outbox.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := outbox.NewRelay(outboxRepository, queuePublisher, outbox.RelayConfig{BatchSize: 10,
		ClaimDuration: 5 * time.Second, MaxAttempts: 3, RetryBase: time.Millisecond, RetryMaximum: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	probe := &searchProbe{}
	processor := newMainPathProcessor(t, pool, eventRepository, probe)
	workerConfig := worker.DefaultConfig()
	workerConfig.ID = "e2e-queue-worker"
	workerConfig.MaxConcurrency = 1
	workerConfig.MaxConcurrencyPerTenant = 1
	workerConfig.PollInterval = 10 * time.Millisecond
	workerConfig.LeaseDuration = 5 * time.Second
	workerConfig.RenewInterval = 2 * time.Second
	workerConfig.RetryDelay = 10 * time.Millisecond
	processErr := make(chan error, 1)
	workerInstance, err := worker.New(queueAdapter, worker.ProcessorFunc(func(ctx context.Context, task worker.Task) worker.Result {
		if err := processor.process(ctx, task.Delivery); err != nil {
			select {
			case processErr <- err:
			default:
			}
			return worker.Result{Disposition: worker.DispositionRetry, RetryAfter: time.Second, Reason: "e2e processor failure"}
		}
		return worker.Result{Disposition: worker.DispositionAck}
	}), workerConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &harness{pool: pool, client: server.Client(), url: server.URL, tenantID: tenantID,
		queue: queueAdapter, relay: relay, worker: workerInstance, processErr: processErr, searched: probe}
}

func (h *harness) dispatch(t *testing.T, ctx context.Context) {
	t.Helper()
	result, err := h.relay.ProcessBatch(ctx, h.tenantID, "e2e-outbox-relay")
	if err != nil || result.Published != 1 {
		t.Fatalf("relay result=%+v error=%v", result, err)
	}
	if err := h.worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.worker.Stop(stopCtx)
	})
}

func (h *harness) assertDurableEffects(t *testing.T, ctx context.Context, created createTurnResponse) {
	t.Helper()
	if h.searched.Count() != 1 {
		t.Fatalf("knowledge.search calls = %d", h.searched.Count())
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var turns, runs, toolSteps, queueAcknowledged int
		err := h.pool.QueryRowContext(ctx, `SELECT
		 (SELECT count(*) FROM turns WHERE tenant_id=$1 AND id=$2),
		 (SELECT count(*) FROM runs WHERE tenant_id=$1 AND id=$3),
		 (SELECT count(*) FROM steps WHERE tenant_id=$1 AND run_id=$3 AND kind='TOOL' AND status='SUCCEEDED'),
		 (SELECT count(*) FROM run_queue_messages WHERE tenant_id=$1 AND run_id=$3 AND status='DONE')`,
			h.tenantID.String(), created.TurnID, created.RunID).Scan(&turns, &runs, &toolSteps, &queueAcknowledged)
		if err == nil && turns == 1 && runs == 1 && toolSteps == 1 && queueAcknowledged == 1 {
			return
		}
		select {
		case processErr := <-h.processErr:
			t.Fatalf("worker processor failed: %v", processErr)
		case <-deadline.C:
			t.Fatalf("durable counts turns=%d runs=%d tools=%d queue_done=%d error=%v", turns, runs, toolSteps, queueAcknowledged, err)
		case <-ticker.C:
		}
	}
}

type mainPathProcessor struct {
	pool       *platformpostgres.Pool
	events     *events.Repository
	lease      *runlease.Manager
	agentStore *agent.PostgresStore
	gateway    *toolexecutor.Executor
	registry   *tools.Registry
	final      *turn.FinalCommitService
}

func newMainPathProcessor(t *testing.T, pool *platformpostgres.Pool, eventRepository *events.Repository, probe *searchProbe) *mainPathProcessor {
	t.Helper()
	execution, err := repository.NewExecution(pool)
	if err != nil {
		t.Fatal(err)
	}
	leaseManager, err := runlease.New(execution, runlease.Config{Owner: "e2e-agent-worker", LeaseDuration: 20 * time.Second, RenewInterval: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	agentStore, err := agent.NewPostgresStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	toolGateway, registry := newToolGateway(t, probe)
	finalStore, err := repository.NewFinalCommitStore(func(ctx context.Context, options *sql.TxOptions, work func(context.Context, repository.DBTX) error) error {
		return pool.WithinTx(ctx, options, func(ctx context.Context, tx platformpostgres.DBTX) error { return work(ctx, tx) })
	})
	if err != nil {
		t.Fatal(err)
	}
	finalService, err := turn.NewFinalCommitService(finalStore)
	if err != nil {
		t.Fatal(err)
	}
	return &mainPathProcessor{pool: pool, events: eventRepository, lease: leaseManager, agentStore: agentStore,
		gateway: toolGateway, registry: registry, final: finalService}
}

func (p *mainPathProcessor) process(ctx context.Context, delivery queue.Delivery) error {
	acquisition, err := p.lease.Acquire(ctx, runlease.AcquireRequest{TenantID: delivery.TenantID, RunID: delivery.RunID})
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = acquisition.Handle.Close(closeCtx)
	}()
	now := time.Now().UTC()
	if _, err := p.events.Append(ctx, events.AppendCommand{TenantID: delivery.TenantID, SessionID: delivery.SessionID,
		TurnID: delivery.TurnID, RunID: delivery.RunID, Type: events.RunStarted, Payload: events.EmptyPayload{},
		OccurredAt: now, Retention: time.Hour}); err != nil {
		return err
	}
	executor := &controlledExecutor{gateway: p.gateway, registry: p.registry, events: p.events,
		tenantID: delivery.TenantID, sessionID: delivery.SessionID, turnID: delivery.TurnID, runID: delivery.RunID}
	machine, err := agent.New(p.agentStore, controlledPlanner{}, executor, &sequenceAgentIDs{}, agent.Config{MaxAttempts: 2})
	if err != nil {
		return err
	}
	result, err := machine.Run(ctx, acquisition)
	if err != nil {
		return err
	}
	var state struct {
		Answer    string   `json:"answer"`
		Citations []string `json:"citations"`
	}
	if err := json.Unmarshal(result.State, &state); err != nil || state.Answer == "" || len(state.Citations) != 1 {
		return fmt.Errorf("invalid controlled Agent result: %w", err)
	}
	var contextVersion, rowVersion uint64
	if err := p.pool.QueryRowContext(ctx, `SELECT context_version,row_version FROM sessions WHERE tenant_id=$1 AND id=$2`,
		delivery.TenantID.String(), delivery.SessionID.String()).Scan(&contextVersion, &rowVersion); err != nil {
		return err
	}
	messageID, _ := domain.ParseMessageID("msg_e2efinal01")
	authority := acquisition.Handle.Snapshot()
	_, err = p.final.Commit(ctx, turn.FinalRequest{TenantID: delivery.TenantID, SessionID: delivery.SessionID,
		TurnID: delivery.TurnID, RunID: delivery.RunID, MessageID: messageID, Content: state.Answer,
		ExpectedContextVersion: contextVersion, ExpectedSessionRowVersion: rowVersion,
		Authority: turn.FinalAuthority{Owner: authority.Owner, Token: authority.Token, ExecutionVersion: authority.ExecutionVersion},
		At:        time.Now().UTC(), EventRetention: time.Hour})
	return err
}

type controlledPlanner struct{}

func (controlledPlanner) Next(_ context.Context, state json.RawMessage, stepNo uint64) (agent.Plan, error) {
	if stepNo == 1 {
		return agent.Plan{Directive: agent.Directive{Kind: domain.StepKindTool, OperationKey: "e2e-knowledge-search",
			Input: json.RawMessage(`{"query":"refund settlement","top_k":1}`), RetryUnknown: true}}, nil
	}
	if stepNo == 2 {
		return agent.Plan{Done: true, State: state}, nil
	}
	return agent.Plan{}, errors.New("unexpected controlled planner step")
}

type controlledExecutor struct {
	gateway   *toolexecutor.Executor
	registry  *tools.Registry
	events    *events.Repository
	tenantID  domain.TenantID
	sessionID domain.SessionID
	turnID    domain.TurnID
	runID     domain.RunID
}

func (e *controlledExecutor) Execute(ctx context.Context, invocation agent.Invocation) (agent.Outcome, error) {
	if invocation.Directive.Kind != domain.StepKindTool {
		return agent.Outcome{}, errors.New("controlled executor accepts Tool steps only")
	}
	tenantID, runID, sessionID, turnID := e.tenantID, e.runID, e.sessionID, e.turnID
	now := time.Now().UTC()
	commands := []events.AppendCommand{
		{TenantID: tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID, Type: events.StepStarted,
			Payload: events.StepStartedPayload{StepID: invocation.StepID, StepNo: invocation.StepNo, Kind: domain.StepKindTool}, OccurredAt: now, Retention: time.Hour},
		{TenantID: tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID, Type: events.ToolStarted,
			Payload: events.ToolStartedPayload{StepID: invocation.StepID, ToolCallID: "tcl_e2e0001", ToolName: builtin.KnowledgeSearchName}, OccurredAt: now, Retention: time.Hour},
	}
	for _, command := range commands {
		if _, err := e.events.Append(ctx, command); err != nil {
			return agent.Outcome{}, err
		}
	}
	registryInvocation, err := toolInvocation(ctx, e.registry, tenantID, sessionID, runID, invocation.Directive.Input)
	if err != nil {
		return agent.Outcome{}, err
	}
	result, err := e.gateway.Execute(ctx, toolexecutor.Request{Invocation: registryInvocation, InvocationKey: "tool-invocation-e2e-0001"})
	if err != nil {
		return agent.Outcome{}, err
	}
	var output struct {
		Items []builtin.KnowledgeItem `json:"items"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil || len(output.Items) != 1 {
		return agent.Outcome{}, errors.New("knowledge Tool returned invalid E2E output")
	}
	completed := []events.AppendCommand{
		{TenantID: tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID, Type: events.RetrievalCompleted,
			Payload: events.RetrievalCompletedPayload{StepID: invocation.StepID, ResultCount: len(output.Items)}, OccurredAt: now, Retention: time.Hour},
		{TenantID: tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID, Type: events.ToolCompleted,
			Payload: events.ToolCompletedPayload{StepID: invocation.StepID, ToolCallID: "tcl_e2e0001", Succeeded: true}, OccurredAt: now, Retention: time.Hour},
		{TenantID: tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID, Type: events.AssistantDelta,
			Payload: events.AssistantDeltaPayload{Text: e2eAnswer}, OccurredAt: now, Retention: time.Hour},
		{TenantID: tenantID, SessionID: sessionID, TurnID: turnID, RunID: runID, Type: events.StepCompleted,
			Payload: events.StepCompletedPayload{StepID: invocation.StepID, StepNo: invocation.StepNo, Status: domain.StepStatusSucceeded}, OccurredAt: now, Retention: time.Hour},
	}
	for _, command := range completed {
		if _, err := e.events.Append(ctx, command); err != nil {
			return agent.Outcome{}, err
		}
	}
	state, _ := json.Marshal(map[string]any{"answer": e2eAnswer,
		"citations": []string{output.Items[0].DocumentID + "/" + output.Items[0].ChunkID}})
	return agent.Outcome{State: state}, nil
}

type schemaContract struct{}

func (schemaContract) Compile(schema json.RawMessage) error {
	if !json.Valid(schema) {
		return errors.New("invalid schema")
	}
	return nil
}

func (schemaContract) Validate(_ json.RawMessage, value json.RawMessage) error {
	if !json.Valid(value) || len(bytes.TrimSpace(value)) == 0 || bytes.TrimSpace(value)[0] != '{' {
		return errors.New("invalid JSON object")
	}
	return nil
}

type allowToolPolicy struct{}

func (allowToolPolicy) Resolve(context.Context, domain.TenantID, string, string) (toolexecutor.Policy, error) {
	return toolexecutor.Policy{Enabled: true, Timeout: time.Second, MaximumOutputBytes: 16 << 10,
		TenantConcurrency: 1, ToolConcurrency: 1}, nil
}

func newToolGateway(t *testing.T, probe *searchProbe) (*toolexecutor.Executor, *tools.Registry) {
	t.Helper()
	registry, err := tools.NewRegistry(schemaContract{})
	if err != nil {
		t.Fatal(err)
	}
	var definition tools.Definition
	for _, candidate := range builtin.Definitions() {
		if candidate.Name == builtin.KnowledgeSearchName {
			definition = candidate
		}
	}
	if err := registry.Register(definition); err != nil {
		t.Fatal(err)
	}
	registry.Seal()
	adapter, err := builtin.NewKnowledgeSearch(probe)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := toolexecutor.New(registry, allowToolPolicy{}, []toolexecutor.Adapter{adapter}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return gateway, registry
}

func toolInvocation(ctx context.Context, registry *tools.Registry, tenantID domain.TenantID,
	sessionID domain.SessionID, runID domain.RunID, arguments json.RawMessage) (tools.Invocation, error) {
	return registry.Resolve(ctx, tools.Call{Name: builtin.KnowledgeSearchName, Version: builtin.Version,
		Scope: tools.Scope{TenantID: tenantID, SessionID: sessionID, RunID: runID}, Permissions: []string{"knowledge:search"}, Arguments: arguments})
}

type searchProbe struct {
	mu    sync.Mutex
	calls int
}

func (s *searchProbe) Search(_ context.Context, query builtin.KnowledgeQuery) ([]builtin.KnowledgeItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if query.TenantID != "ten_e2e0001" || query.SessionID != "ses_e2e0001" || query.Query != "refund settlement" || query.TopK != 1 {
		return nil, errors.New("knowledge query escaped its tenant or Run scope")
	}
	return []builtin.KnowledgeItem{{DocumentID: "doc_refund", ChunkID: "chk_refund",
		Content: "Refunds settle in five business days.", Score: 0.99}}, nil
}

func (s *searchProbe) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type sequenceAgentIDs struct {
	mu       sync.Mutex
	steps    int
	attempts int
}

func (s *sequenceAgentIDs) NewStepID() (domain.StepID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps++
	return domain.ParseStepID(fmt.Sprintf("stp_e2e%04d", s.steps))
}

func (s *sequenceAgentIDs) NewAttemptID() (domain.AttemptID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	return domain.ParseAttemptID(fmt.Sprintf("att_e2e%04d", s.attempts))
}

type pollingSubscriber struct{}

func (pollingSubscriber) Subscribe(context.Context, domain.TenantID, domain.RunID, uint64) (<-chan events.Event, <-chan error) {
	values := make(chan events.Event)
	failures := make(chan error)
	close(values)
	close(failures)
	return values, failures
}

type sseEvent struct {
	ID   uint64
	Type string
}

func openSSE(t *testing.T, client *http.Client, url, after string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+e2eToken)
	request.Header.Set("Accept", "text/event-stream")
	if after != "" {
		request.Header.Set("Last-Event-ID", after)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("SSE status=%d content-type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), body)
	}
	return response
}

func readSSEEvent(t *testing.T, reader io.Reader) sseEvent {
	t.Helper()
	scanner := bufio.NewScanner(reader)
	result := sseEvent{}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			result.ID, _ = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			result.Type = strings.TrimPrefix(line, "event: ")
		case line == "" && result.ID > 0:
			return result
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SSE event: %v", err)
	}
	t.Fatal("SSE stream ended before an event")
	return sseEvent{}
}

func readUntilTerminal(t *testing.T, reader io.Reader) []sseEvent {
	t.Helper()
	scanner := bufio.NewScanner(reader)
	var result []sseEvent
	current := sseEvent{}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			current.ID, _ = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			current.Type = strings.TrimPrefix(line, "event: ")
		case line == "" && current.ID > 0:
			result = append(result, current)
			if current.Type == events.RunCompleted.String() {
				return result
			}
			current = sseEvent{}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read resumed SSE: %v", err)
	}
	t.Fatalf("resumed SSE ended without terminal event: %+v", result)
	return nil
}

func assertContiguousResume(t *testing.T, first sseEvent, remaining []sseEvent) {
	t.Helper()
	if len(remaining) == 0 || remaining[len(remaining)-1].Type != events.RunCompleted.String() {
		t.Fatalf("terminal events = %+v", remaining)
	}
	previous := first.ID
	types := map[string]bool{first.Type: true}
	for _, event := range remaining {
		if event.ID != previous+1 {
			t.Fatalf("SSE resume sequence jumped from %d to %d", previous, event.ID)
		}
		previous = event.ID
		types[event.Type] = true
	}
	for _, required := range []string{events.ToolStarted.String(), events.ToolCompleted.String(), events.RetrievalCompleted.String(), events.RunCompleted.String()} {
		if !types[required] {
			t.Errorf("SSE stream omitted %s", required)
		}
	}
}

func doJSON[T any](t *testing.T, client *http.Client, method, url, idempotencyKey, body string, status int) T {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+e2eToken)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, url, response.StatusCode, status, data)
	}
	var decoded T
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode %s %s: %v body=%s", method, url, err, data)
	}
	return decoded
}

func doFailure(t *testing.T, client *http.Client, method, url, idempotencyKey, body string, status int) publicFailure {
	return doJSON[publicFailure](t, client, method, url, idempotencyKey, body, status)
}

func fixedTurnIDs() (turn.IDs, error) {
	turnID, err := domain.ParseTurnID("trn_e2e0001")
	if err != nil {
		return turn.IDs{}, err
	}
	messageID, err := domain.ParseMessageID("msg_e2euser01")
	if err != nil {
		return turn.IDs{}, err
	}
	runID, err := domain.ParseRunID("run_e2e0001")
	if err != nil {
		return turn.IDs{}, err
	}
	return turn.IDs{TurnID: turnID, MessageID: messageID, RunID: runID, EventID: "evt_e2e0001"}, nil
}

func registerPostgres(t *testing.T, container testcontainers.Container) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if logs, err := container.Logs(ctx); err == nil {
				data, _ := io.ReadAll(io.LimitReader(logs, 256<<10))
				_ = logs.Close()
				t.Logf("PostgreSQL log tail (bounded):\n%s", data)
			}
		}
		testcontainers.CleanupContainer(t, container)
	})
}
