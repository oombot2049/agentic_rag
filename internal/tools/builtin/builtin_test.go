package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
	"github.com/agenticrag/agenticrag/internal/tools/executor"
)

type jsonObjectSchemas struct{}

func (jsonObjectSchemas) Compile(schema json.RawMessage) error {
	var value map[string]any
	return json.Unmarshal(schema, &value)
}
func (jsonObjectSchemas) Validate(_ json.RawMessage, payload json.RawMessage) error {
	var value map[string]any
	return json.Unmarshal(payload, &value)
}

type policyFunc func(context.Context, domain.TenantID, string, string) (executor.Policy, error)

func (f policyFunc) Resolve(ctx context.Context, tenant domain.TenantID, name, version string) (executor.Policy, error) {
	return f(ctx, tenant, name, version)
}

type searcherStub struct {
	query KnowledgeQuery
	items []KnowledgeItem
	err   error
	calls int
}

func (s *searcherStub) Search(_ context.Context, query KnowledgeQuery) ([]KnowledgeItem, error) {
	s.calls++
	s.query = query
	return append([]KnowledgeItem(nil), s.items...), s.err
}

type idempotentCreator struct {
	mu      sync.Mutex
	results map[string]TicketResult
	inputs  map[string]TicketCommand
	creates int
	err     error
}

func (c *idempotentCreator) Create(_ context.Context, command TicketCommand) (TicketResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return TicketResult{}, c.err
	}
	if existing, ok := c.results[command.IdempotencyKey]; ok {
		if c.inputs[command.IdempotencyKey] != command {
			return TicketResult{}, errors.New("idempotency conflict")
		}
		return existing, nil
	}
	c.creates++
	result := TicketResult{TicketID: "tic_0001", Status: "OPEN"}
	c.results[command.IdempotencyKey], c.inputs[command.IdempotencyKey] = result, command
	return result, nil
}

func TestDefinitionAndCapabilityContracts(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != 2 {
		t.Fatalf("definitions=%d", len(definitions))
	}
	want := map[string]struct {
		permission string
		effect     tools.Effect
	}{
		KnowledgeSearchName: {"knowledge:search", tools.EffectReadOnly},
		TicketCreateName:    {"support:ticket:create", tools.EffectIdempotentWrite},
	}
	for _, definition := range definitions {
		expected, ok := want[definition.Name]
		if !ok || definition.Version != Version || definition.Scope != tools.ScopeRun || definition.Effect != expected.effect ||
			len(definition.RequiredPermissions) != 1 || definition.RequiredPermissions[0] != expected.permission {
			t.Fatalf("definition=%+v", definition)
		}
		for _, schema := range []json.RawMessage{definition.InputSchema, definition.OutputSchema} {
			var object map[string]any
			if json.Unmarshal(schema, &object) != nil || object["type"] != "object" || object["additionalProperties"] != false {
				t.Fatalf("open schema: %s", schema)
			}
		}
	}
	searcher := &searcherStub{}
	creator := &idempotentCreator{results: map[string]TicketResult{}, inputs: map[string]TicketCommand{}}
	adapters, err := Adapters(searcher, creator)
	if err != nil || len(adapters) != 2 {
		t.Fatalf("adapters=%d err=%v", len(adapters), err)
	}
	for _, adapter := range adapters {
		if adapter.Version() != Version || adapter.Capabilities().Network != executor.NetworkNone || adapter.Capabilities().IsolatedCommand || len(adapter.Capabilities().Destinations) != 0 {
			t.Fatalf("adapter=%s capabilities=%+v", adapter.Name(), adapter.Capabilities())
		}
	}
	if _, err = Adapters(nil, creator); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = NewTicketCreate(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestExamplesExecuteThroughRegistryExecutorAndLedger(t *testing.T) {
	searcher := &searcherStub{items: []KnowledgeItem{{DocumentID: "doc_0001", ChunkID: "chk_0001", Content: "bounded evidence", Score: 0.91}}}
	creator := &idempotentCreator{results: make(map[string]TicketResult), inputs: make(map[string]TicketCommand)}
	registry, err := tools.NewRegistry(jsonObjectSchemas{})
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range Definitions() {
		if err = registry.Register(definition); err != nil {
			t.Fatal(err)
		}
	}
	registry.Seal()
	adapters, _ := Adapters(searcher, creator)
	policy := policyFunc(func(context.Context, domain.TenantID, string, string) (executor.Policy, error) {
		return executor.Policy{Enabled: true, Timeout: time.Second, MaximumOutputBytes: 32 << 10, TenantConcurrency: 4, ToolConcurrency: 2}, nil
	})
	runner, err := executor.New(registry, policy, adapters, 8)
	if err != nil {
		t.Fatal(err)
	}
	scope := tools.Scope{TenantID: "ten_0001", SessionID: "ses_0001", RunID: "run_0001"}
	knowledge, err := registry.Resolve(context.Background(), tools.Call{Name: KnowledgeSearchName, Version: Version, Scope: scope,
		Permissions: []string{"knowledge:search"}, Arguments: json.RawMessage(`{"query":"refund policy","top_k":3}`)})
	if err != nil {
		t.Fatal(err)
	}
	knowledgeResult, err := runner.Execute(context.Background(), executor.Request{Invocation: knowledge, InvocationKey: stableInvocationKey()})
	if err != nil || !strings.Contains(string(knowledgeResult.Output), "bounded evidence") || searcher.calls != 1 || searcher.query.TopK != 3 || searcher.query.TenantID != "ten_0001" {
		t.Fatalf("output=%s query=%+v err=%v", knowledgeResult.Output, searcher.query, err)
	}

	ticket, err := registry.Resolve(context.Background(), tools.Call{Name: TicketCreateName, Version: Version, Scope: scope,
		Permissions: []string{"support:ticket:create"}, Arguments: json.RawMessage(`{"title":"Cannot sign in","description":"Account is locked","priority":"HIGH"}`)})
	if err != nil {
		t.Fatal(err)
	}
	store := tools.NewMemoryLedgerStore()
	keys, _ := tools.NewHMACKeyDeriver([]byte("01234567890123456789012345678901"))
	ledger, _ := tools.NewLedger(store, keys)
	at := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	command := tools.BeginCommand{CallID: "tcl_0001", RunID: "run_0001", StepID: "stp_0001", AttemptID: "att_0001", AttemptNo: 1, Invocation: ticket, At: at}
	begin, err := ledger.Begin(context.Background(), command)
	if err != nil || begin.Decision != tools.DecisionExecute {
		t.Fatalf("begin=%+v err=%v", begin, err)
	}
	created, err := runner.Execute(context.Background(), executor.Request{Invocation: ticket, InvocationKey: begin.Call.InvocationKey})
	if err != nil {
		t.Fatal(err)
	}
	retried, err := runner.Execute(context.Background(), executor.Request{Invocation: ticket, InvocationKey: begin.Call.InvocationKey})
	if err != nil || string(retried.Output) != string(created.Output) {
		t.Fatalf("idempotent retry=%s err=%v", retried.Output, err)
	}
	if creator.creates != 1 || creator.inputs[begin.Call.InvocationKey].TenantID != "ten_0001" {
		t.Fatalf("creator=%+v", creator)
	}
	if _, err = ledger.RecordOutcome(context.Background(), tools.OutcomeCommand{TenantID: "ten_0001", CallID: "tcl_0001", AttemptID: "att_0001",
		ExpectedRowVersion: begin.Call.RowVersion, Status: tools.CallSucceeded, Result: created.Output, At: at.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	replay, err := ledger.Begin(context.Background(), command)
	if err != nil || replay.Decision != tools.DecisionReturnResult || string(replay.Call.Result) != string(created.Output) || creator.creates != 1 {
		t.Fatalf("replay=%+v creates=%d err=%v", replay, creator.creates, err)
	}
}

func TestAdaptersRejectInvalidRequestsAndBackendResults(t *testing.T) {
	valid := adapterRequest(KnowledgeSearchName, json.RawMessage(`{"query":"hello"}`))
	searcher := &searcherStub{items: []KnowledgeItem{{DocumentID: "bad", ChunkID: "chk_0001", Content: "text", Score: 1}}}
	knowledge, _ := NewKnowledgeSearch(searcher)
	if _, err := knowledge.Execute(context.Background(), valid); !errors.Is(err, ErrBackend) {
		t.Fatalf("invalid result=%v", err)
	}
	searcher.items = make([]KnowledgeItem, 6)
	if _, err := knowledge.Execute(context.Background(), valid); !errors.Is(err, ErrBackend) {
		t.Fatalf("unbounded result=%v", err)
	}
	searcher.err = errors.New("secret backend error")
	if _, err := knowledge.Execute(context.Background(), valid); !errors.Is(err, ErrBackend) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("backend=%v", err)
	}
	for _, request := range []executor.AdapterRequest{
		adapterRequest("wrong.name", json.RawMessage(`{"query":"hello"}`)),
		adapterRequest(KnowledgeSearchName, json.RawMessage(`{"query":"hello","secret":"x"}`)),
		adapterRequest(KnowledgeSearchName, json.RawMessage(`{"query":" "}`)),
	} {
		if _, err := knowledge.Execute(context.Background(), request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("request=%+v err=%v", request, err)
		}
	}

	creator := &idempotentCreator{results: make(map[string]TicketResult), inputs: make(map[string]TicketCommand)}
	ticket, _ := NewTicketCreate(creator)
	ticketRequest := adapterRequest(TicketCreateName, json.RawMessage(`{"title":"Help","description":"Details","priority":"URGENT"}`))
	if _, err := ticket.Execute(context.Background(), ticketRequest); !errors.Is(err, ErrInvalid) {
		t.Fatalf("priority=%v", err)
	}
	ticketRequest.Arguments = json.RawMessage(`{"title":"Help","description":"Details","priority":"NORMAL"}`)
	ticketRequest.InvocationKey = "bad"
	if _, err := ticket.Execute(context.Background(), ticketRequest); !errors.Is(err, ErrInvalid) {
		t.Fatalf("key=%v", err)
	}
	creator.results[stableInvocationKey()] = TicketResult{TicketID: "wrong", Status: "OPEN"}
	creator.inputs[stableInvocationKey()] = TicketCommand{TenantID: "ten_0001", SessionID: "ses_0001", RunID: "run_0001", IdempotencyKey: stableInvocationKey(), Title: "Help", Description: "Details", Priority: "NORMAL"}
	ticketRequest.InvocationKey = stableInvocationKey()
	if _, err := ticket.Execute(context.Background(), ticketRequest); !errors.Is(err, ErrBackend) {
		t.Fatalf("result=%v", err)
	}
}

func adapterRequest(name string, arguments json.RawMessage) executor.AdapterRequest {
	return executor.AdapterRequest{TenantID: "ten_0001", Name: name, Version: Version,
		Scope: tools.Scope{TenantID: "ten_0001", SessionID: "ses_0001", RunID: "run_0001"}, Arguments: arguments, InvocationKey: stableInvocationKey()}
}

func stableInvocationKey() string { return "tik_" + strings.Repeat("A", 43) }
