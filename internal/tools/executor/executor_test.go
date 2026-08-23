package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
)

type outputSchemaContract struct{}

func (outputSchemaContract) Compile(json.RawMessage) error { return nil }
func (outputSchemaContract) Validate(schema json.RawMessage, payload json.RawMessage) error {
	if !strings.Contains(string(schema), `"required":["answer"]`) {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(payload, &value) != nil || value["answer"] == nil {
		return errors.New("missing answer")
	}
	return nil
}

type policyResolver func(context.Context, domain.TenantID, string, string) (Policy, error)

func (f policyResolver) Resolve(ctx context.Context, tenant domain.TenantID, name, version string) (Policy, error) {
	return f(ctx, tenant, name, version)
}

type adapterFunc struct {
	capabilities Capabilities
	execute      func(context.Context, AdapterRequest) (io.ReadCloser, error)
}

func (a *adapterFunc) Name() string               { return "knowledge.search" }
func (a *adapterFunc) Version() string            { return "1.0.0" }
func (a *adapterFunc) Capabilities() Capabilities { return a.capabilities }
func (a *adapterFunc) Execute(ctx context.Context, request AdapterRequest) (io.ReadCloser, error) {
	return a.execute(ctx, request)
}

func TestExecutorSuccessValidatesAndDefensivelyCopies(t *testing.T) {
	registry, invocation := testInvocation(t, "ten_0001")
	adapter := &adapterFunc{capabilities: Capabilities{Network: NetworkNone}, execute: func(_ context.Context, request AdapterRequest) (io.ReadCloser, error) {
		if request.InvocationKey != "tik_stable" || request.TenantID != "ten_0001" {
			t.Errorf("request = %+v", request)
		}
		request.Arguments[0] = '['
		return io.NopCloser(strings.NewReader(`{"answer":"ok"}`)), nil
	}}
	executor, err := New(registry, staticPolicy(validPolicy()), []Adapter{adapter}, 4)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), Request{Invocation: invocation, InvocationKey: "tik_stable"})
	if err != nil || string(result.Output) != `{"answer":"ok"}` {
		t.Fatalf("result = %s, %v", result.Output, err)
	}
	result.Output[0] = '['
	if invocation.Arguments[0] != '{' {
		t.Fatal("adapter mutated Invocation arguments")
	}
}

func TestExecutorDenyByDefaultAndCapabilities(t *testing.T) {
	registry, invocation := testInvocation(t, "ten_0001")
	network := &adapterFunc{capabilities: Capabilities{Network: NetworkBrokered, Destinations: []string{"api.example.com"}}, execute: successAdapter}
	tests := []struct {
		name   string
		policy Policy
		want   error
	}{
		{"disabled", Policy{}, ErrDenied},
		{"network disabled", validPolicy(), ErrDenied},
		{"host absent", withNetwork(validPolicy(), "other.example.com"), ErrDenied},
		{"allowed", withNetwork(validPolicy(), "api.example.com"), nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, err := New(registry, staticPolicy(test.policy), []Adapter{network}, 2)
			if err != nil {
				t.Fatal(err)
			}
			_, err = executor.Execute(context.Background(), Request{Invocation: invocation, InvocationKey: "tik_stable"})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	command := &adapterFunc{capabilities: Capabilities{Network: NetworkNone, IsolatedCommand: true}, execute: successAdapter}
	executor, _ := New(registry, staticPolicy(validPolicy()), []Adapter{command}, 1)
	if _, err := executor.Execute(context.Background(), Request{Invocation: invocation, InvocationKey: "tik"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("command = %v", err)
	}
	invalidNetwork := &adapterFunc{capabilities: Capabilities{Network: NetworkBrokered, Destinations: []string{"127.0.0.1"}}, execute: successAdapter}
	if _, err := New(registry, staticPolicy(validPolicy()), []Adapter{invalidNetwork}, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("IP capability = %v", err)
	}
}

func TestExecutorRejectsInvocationMutationBeforeAdapter(t *testing.T) {
	registry, invocation := testInvocation(t, "ten_0001")
	var calls atomic.Int64
	adapter := &adapterFunc{capabilities: Capabilities{Network: NetworkNone}, execute: func(context.Context, AdapterRequest) (io.ReadCloser, error) {
		calls.Add(1)
		return io.NopCloser(strings.NewReader(`{"answer":"ok"}`)), nil
	}}
	executor, _ := New(registry, staticPolicy(validPolicy()), []Adapter{adapter}, 1)
	invocation.Arguments = json.RawMessage(`{"query":"tampered"}`)
	if _, err := executor.Execute(context.Background(), Request{Invocation: invocation, InvocationKey: "tik"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("mutated Invocation = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("adapter called for mutated Invocation")
	}
}

func TestExecutorTimeoutKeepsPermitUntilFaultyAdapterExits(t *testing.T) {
	registry, invocation := testInvocation(t, "ten_0001")
	releaseAdapter := make(chan struct{})
	var calls atomic.Int64
	adapter := &adapterFunc{capabilities: Capabilities{Network: NetworkNone}, execute: func(context.Context, AdapterRequest) (io.ReadCloser, error) {
		calls.Add(1)
		<-releaseAdapter // intentionally ignores cancellation
		return io.NopCloser(strings.NewReader(`{"answer":"late"}`)), nil
	}}
	policy := validPolicy()
	policy.Timeout = 35 * time.Millisecond
	policy.TenantConcurrency = 1
	policy.ToolConcurrency = 1
	executor, _ := New(registry, staticPolicy(policy), []Adapter{adapter}, 1)
	request := Request{Invocation: invocation, InvocationKey: "tik"}
	if _, err := executor.Execute(context.Background(), request); !errors.Is(err, ErrTimeout) {
		t.Fatalf("first = %v", err)
	}
	if _, err := executor.Execute(context.Background(), request); !errors.Is(err, ErrCapacity) {
		t.Fatalf("permit leaked open = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("adapter calls = %d", calls.Load())
	}
	close(releaseAdapter)
	deadline := time.Now().Add(time.Second)
	for executor.limits.inUse() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if executor.limits.inUse() != 0 {
		t.Fatal("permit was not released after adapter exit")
	}
}

func TestExecutorOutputLimitsSchemaErrorsAndReaderClose(t *testing.T) {
	registry, invocation := testInvocation(t, "ten_0001")
	tests := []struct {
		name, output string
		adapterErr   error
		want         error
	}{
		{"oversized", strings.Repeat("x", 33), nil, ErrOutputLimit},
		{"schema", `{"other":1}`, nil, ErrInvalidOutput},
		{"adapter", "", errors.New("secret provider detail"), ErrAdapter},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &trackingReader{Reader: strings.NewReader(test.output)}
			adapter := &adapterFunc{capabilities: Capabilities{Network: NetworkNone}, execute: func(context.Context, AdapterRequest) (io.ReadCloser, error) {
				if test.adapterErr != nil {
					return nil, test.adapterErr
				}
				return reader, nil
			}}
			policy := validPolicy()
			policy.MaximumOutputBytes = 32
			executor, _ := New(registry, staticPolicy(policy), []Adapter{adapter}, 1)
			_, err := executor.Execute(context.Background(), Request{Invocation: invocation, InvocationKey: "tik"})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v", err)
			}
			if test.adapterErr == nil && !reader.closed.Load() {
				t.Fatal("reader was not closed")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("raw adapter error leaked")
			}
		})
	}
}

func TestExecutorConcurrencyAndCancellation(t *testing.T) {
	registry, invocation := testInvocation(t, "ten_0001")
	var active, peak atomic.Int64
	adapter := &adapterFunc{capabilities: Capabilities{Network: NetworkNone}, execute: func(ctx context.Context, _ AdapterRequest) (io.ReadCloser, error) {
		current := active.Add(1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		defer active.Add(-1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
		return io.NopCloser(strings.NewReader(`{"answer":"ok"}`)), nil
	}}
	policy := validPolicy()
	policy.TenantConcurrency = 2
	policy.ToolConcurrency = 2
	executor, _ := New(registry, staticPolicy(policy), []Adapter{adapter}, 2)
	var wait sync.WaitGroup
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = executor.Execute(context.Background(), Request{Invocation: invocation, InvocationKey: "tik"})
		}()
	}
	wait.Wait()
	if peak.Load() > 2 || executor.limits.inUse() != 0 {
		t.Fatalf("peak=%d global=%d", peak.Load(), executor.limits.inUse())
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executor.Execute(canceled, Request{Invocation: invocation, InvocationKey: "tik"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pre-canceled = %v", err)
	}
}

type trackingReader struct {
	*strings.Reader
	closed atomic.Bool
}

func (r *trackingReader) Close() error { r.closed.Store(true); return nil }

func successAdapter(context.Context, AdapterRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(`{"answer":"ok"}`)), nil
}

func validPolicy() Policy {
	return Policy{Enabled: true, Timeout: time.Second, MaximumOutputBytes: 1024,
		TenantConcurrency: 4, ToolConcurrency: 2}
}
func withNetwork(policy Policy, host string) Policy {
	policy.AllowBrokeredNetwork = true
	policy.AllowedHosts = []string{host}
	return policy
}
func staticPolicy(policy Policy) PolicyResolver {
	return policyResolver(func(context.Context, domain.TenantID, string, string) (Policy, error) { return policy, nil })
}

func testInvocation(t *testing.T, tenant domain.TenantID) (*tools.Registry, tools.Invocation) {
	t.Helper()
	registry, _ := tools.NewRegistry(outputSchemaContract{})
	definition := tools.Definition{Name: "knowledge.search", Version: "1.0.0", Description: "Search knowledge",
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object","required":["answer"]}`),
		RequiredPermissions: []string{"knowledge.read"}, Scope: tools.ScopeRun, Effect: tools.EffectReadOnly}
	if err := registry.Register(definition); err != nil {
		t.Fatal(err)
	}
	registry.Seal()
	invocation, err := registry.Resolve(context.Background(), tools.Call{Name: definition.Name, Version: definition.Version,
		Scope:       tools.Scope{TenantID: tenant, SessionID: "ses_0001", RunID: "run_0001"},
		Permissions: []string{"knowledge.read"}, Arguments: json.RawMessage(`{"query":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return registry, invocation
}
