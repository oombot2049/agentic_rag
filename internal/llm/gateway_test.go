package llm

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var llmTestTime = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func TestGatewayStructuredSuccessAccountingAndCopies(t *testing.T) {
	recorder := &recordingRecorder{}
	validatorCalls := 0
	validator := StructuredValidatorFunc(func(schema, value json.RawMessage) error {
		validatorCalls++
		if string(schema) != `{"type":"object"}` || string(value) != `{"answer":"ok"}` {
			t.Fatalf("schema=%s value=%s", schema, value)
		}
		schema[0], value[0] = 'x', 'x'
		return nil
	})
	provider := ProviderFunc(func(ctx context.Context, request ProviderRequest) (ProviderResponse, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("provider attempt has no deadline")
		}
		request.Messages[0].Content = "provider mutation"
		request.ResponseSchema[0] = 'x'
		return ProviderResponse{Structured: json.RawMessage(`{"answer":"ok"}`), FinishReason: FinishReasonStop,
			Usage: Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}, ProviderRequestID: "provider-request-1"}, nil
	})
	gateway := testGateway(t, provider, Price{InputNanosPerMillionTokens: 2_000_000_000,
		OutputNanosPerMillionTokens: 3_000_000_000}, validator, recorder, testGatewayConfig())
	request := validStructuredRequest()

	result, err := gateway.Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempts != 1 || result.CostNanos != 12_000 || string(result.Response.Structured) != `{"answer":"ok"}` || validatorCalls != 1 {
		t.Fatalf("result=%+v validator calls=%d", result, validatorCalls)
	}
	if request.Messages[0].Content != "policy" || string(request.ResponseSchema) != `{"type":"object"}` {
		t.Fatalf("provider mutated request=%+v", request)
	}
	result.Response.Structured[0] = 'x'
	records := recorder.values()
	if len(records) != 1 || !records[0].Succeeded || records[0].Attempts != 1 || records[0].CostNanos != 12_000 || records[0].Usage.TotalTokens != 5 {
		t.Fatalf("records=%+v", records)
	}
}

func TestGatewayRetriesOnlyExplicitProviderFailures(t *testing.T) {
	retryable := []ErrorKind{ErrorRateLimited, ErrorTimeout, ErrorUnavailable}
	for _, kind := range retryable {
		t.Run(string(kind), func(t *testing.T) {
			calls := 0
			sleeper := &fakeSleeper{}
			configuration := testGatewayConfig()
			configuration.Sleeper = sleeper
			provider := ProviderFunc(func(context.Context, ProviderRequest) (ProviderResponse, error) {
				calls++
				if calls == 1 {
					return ProviderResponse{}, providerFailure(t, kind, 0)
				}
				return validTextResponse(), nil
			})
			gateway := testGateway(t, provider, Price{}, acceptingValidator(), &recordingRecorder{}, configuration)
			result, err := gateway.Generate(context.Background(), validTextRequest())
			if err != nil || result.Attempts != 2 || calls != 2 || len(sleeper.delays) != 1 {
				t.Fatalf("result=%+v error=%v calls=%d delays=%v", result, err, calls, sleeper.delays)
			}
		})
	}

	nonRetryable := []ErrorKind{ErrorInvalidRequest, ErrorAuthentication, ErrorSafety, ErrorBadResponse, ErrorAdapter}
	for _, kind := range nonRetryable {
		t.Run(string(kind), func(t *testing.T) {
			calls := 0
			provider := ProviderFunc(func(context.Context, ProviderRequest) (ProviderResponse, error) {
				calls++
				return ProviderResponse{}, providerFailure(t, kind, 0)
			})
			gateway := testGateway(t, provider, Price{}, acceptingValidator(), &recordingRecorder{}, testGatewayConfig())
			_, err := gateway.Generate(context.Background(), validTextRequest())
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.Kind() != kind || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestGatewayRetryBackoffRetryAfterAndAttemptLimit(t *testing.T) {
	sleeper := &fakeSleeper{}
	configuration := testGatewayConfig()
	configuration.MaxAttempts = 3
	configuration.RetryBase = 10 * time.Millisecond
	configuration.RetryMaximum = 40 * time.Millisecond
	configuration.Sleeper = sleeper
	configuration.Random = maximumRandom{}
	calls := 0
	provider := ProviderFunc(func(context.Context, ProviderRequest) (ProviderResponse, error) {
		calls++
		return ProviderResponse{}, providerFailure(t, ErrorRateLimited, 50*time.Millisecond)
	})
	recorder := &recordingRecorder{}
	gateway := testGateway(t, provider, Price{}, acceptingValidator(), recorder, configuration)

	_, err := gateway.Generate(context.Background(), validTextRequest())
	var providerErr *ProviderError
	if !errors.As(err, &providerErr) || calls != 3 || len(sleeper.delays) != 2 ||
		sleeper.delays[0] != 40*time.Millisecond || sleeper.delays[1] != 40*time.Millisecond {
		t.Fatalf("error=%v calls=%d delays=%v", err, calls, sleeper.delays)
	}
	records := recorder.values()
	if len(records) != 1 || records[0].Succeeded || records[0].Attempts != 3 || records[0].FailureKind != ErrorRateLimited {
		t.Fatalf("records=%+v", records)
	}
}

func TestGatewayAppliesAttemptTimeoutAndCallerCancellation(t *testing.T) {
	t.Run("attempt timeout", func(t *testing.T) {
		configuration := testGatewayConfig()
		configuration.AttemptTimeout = 2 * time.Millisecond
		configuration.MaxAttempts = 2
		configuration.Sleeper = &fakeSleeper{}
		var calls atomic.Int32
		provider := ProviderFunc(func(ctx context.Context, _ ProviderRequest) (ProviderResponse, error) {
			calls.Add(1)
			<-ctx.Done()
			return ProviderResponse{}, ctx.Err()
		})
		gateway := testGateway(t, provider, Price{}, acceptingValidator(), &recordingRecorder{}, configuration)
		_, err := gateway.Generate(context.Background(), validTextRequest())
		var providerErr *ProviderError
		if !errors.As(err, &providerErr) || providerErr.Kind() != ErrorTimeout || calls.Load() != 2 {
			t.Fatalf("error=%v calls=%d", err, calls.Load())
		}
	})

	t.Run("cancel during backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		sleeper := &fakeSleeper{hook: cancel}
		configuration := testGatewayConfig()
		configuration.Sleeper = sleeper
		provider := ProviderFunc(func(context.Context, ProviderRequest) (ProviderResponse, error) {
			return ProviderResponse{}, providerFailure(t, ErrorUnavailable, 0)
		})
		gateway := testGateway(t, provider, Price{}, acceptingValidator(), &recordingRecorder{}, configuration)
		_, err := gateway.Generate(ctx, validTextRequest())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
		snapshot, _ := gateway.BreakerSnapshot("provider-a")
		if snapshot.State != BreakerClosed || snapshot.ConsecutiveFailures != 0 {
			t.Fatalf("breaker=%+v", snapshot)
		}
	})
}

func TestGatewayCircuitBreakerSingleHalfOpenProbe(t *testing.T) {
	clock := &fakeClock{now: llmTestTime}
	configuration := testGatewayConfig()
	configuration.Clock = clock
	configuration.MaxAttempts = 1
	configuration.FailureThreshold = 2
	configuration.OpenDuration = time.Minute
	releaseProbe := make(chan struct{})
	probeStarted := make(chan struct{})
	var calls atomic.Int32
	provider := ProviderFunc(func(context.Context, ProviderRequest) (ProviderResponse, error) {
		call := calls.Add(1)
		if call <= 2 {
			return ProviderResponse{}, providerFailure(t, ErrorUnavailable, 0)
		}
		close(probeStarted)
		<-releaseProbe
		return validTextResponse(), nil
	})
	gateway := testGateway(t, provider, Price{}, acceptingValidator(), &recordingRecorder{}, configuration)
	for index := 0; index < 2; index++ {
		if _, err := gateway.Generate(context.Background(), validTextRequest()); err == nil {
			t.Fatal("expected provider failure")
		}
	}
	if _, err := gateway.Generate(context.Background(), validTextRequest()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open error=%v", err)
	}
	clock.advance(time.Minute)
	probeDone := make(chan error, 1)
	go func() {
		_, err := gateway.Generate(context.Background(), validTextRequest())
		probeDone <- err
	}()
	<-probeStarted

	const contenders = 32
	var wait sync.WaitGroup
	var rejected atomic.Int32
	wait.Add(contenders)
	for index := 0; index < contenders; index++ {
		go func() {
			defer wait.Done()
			if _, err := gateway.Generate(context.Background(), validTextRequest()); errors.Is(err, ErrCircuitOpen) {
				rejected.Add(1)
			}
		}()
	}
	wait.Wait()
	if rejected.Load() != contenders || calls.Load() != 3 {
		t.Fatalf("rejected=%d provider calls=%d", rejected.Load(), calls.Load())
	}
	close(releaseProbe)
	if err := <-probeDone; err != nil {
		t.Fatal(err)
	}
	snapshot, _ := gateway.BreakerSnapshot("provider-a")
	if snapshot.State != BreakerClosed || snapshot.ConsecutiveFailures != 0 || snapshot.ProbeInFlight {
		t.Fatalf("breaker=%+v", snapshot)
	}
}

func TestGatewayRejectsInvalidProviderResponsesAndSchema(t *testing.T) {
	validationError := errors.New("schema mismatch")
	tests := []struct {
		name      string
		request   Request
		response  ProviderResponse
		validator StructuredValidator
		wantKind  ErrorKind
	}{
		{name: "usage mismatch", request: validTextRequest(), response: ProviderResponse{Text: "ok", FinishReason: FinishReasonStop, Usage: Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 3}}, validator: acceptingValidator(), wantKind: ErrorBadResponse},
		{name: "output exceeds request", request: validTextRequest(), response: ProviderResponse{Text: "ok", FinishReason: FinishReasonStop, Usage: Usage{InputTokens: 1, OutputTokens: 101, TotalTokens: 102}}, validator: acceptingValidator(), wantKind: ErrorBadResponse},
		{name: "unknown finish", request: validTextRequest(), response: ProviderResponse{Text: "ok", FinishReason: "UNKNOWN", Usage: Usage{}}, validator: acceptingValidator(), wantKind: ErrorBadResponse},
		{name: "structured contains text", request: validStructuredRequest(), response: ProviderResponse{Text: "leak", Structured: json.RawMessage(`{}`), FinishReason: FinishReasonStop, Usage: Usage{}}, validator: acceptingValidator(), wantKind: ErrorBadResponse},
		{name: "schema mismatch", request: validStructuredRequest(), response: ProviderResponse{Structured: json.RawMessage(`{}`), FinishReason: FinishReasonStop, Usage: Usage{}}, validator: StructuredValidatorFunc(func(json.RawMessage, json.RawMessage) error { return validationError }), wantKind: ErrorBadResponse},
		{name: "safety finish", request: validTextRequest(), response: ProviderResponse{FinishReason: FinishReasonSafety, Usage: Usage{}}, validator: acceptingValidator(), wantKind: ErrorSafety},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gateway := testGateway(t, ProviderFunc(func(context.Context, ProviderRequest) (ProviderResponse, error) {
				return test.response, nil
			}), Price{}, test.validator, &recordingRecorder{}, testGatewayConfig())
			_, err := gateway.Generate(context.Background(), test.request)
			var providerErr *ProviderError
			if !errors.As(err, &providerErr) || providerErr.Kind() != test.wantKind {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGatewayValidationRoutesAndCostOverflow(t *testing.T) {
	providerCalls := 0
	provider := ProviderFunc(func(context.Context, ProviderRequest) (ProviderResponse, error) {
		providerCalls++
		return ProviderResponse{Text: "ok", FinishReason: FinishReasonStop,
			Usage: Usage{InputTokens: 10_000_000, TotalTokens: 10_000_000}}, nil
	})
	gateway := testGateway(t, provider, Price{InputNanosPerMillionTokens: math.MaxInt64}, acceptingValidator(), &recordingRecorder{}, testGatewayConfig())
	if _, err := gateway.Generate(context.Background(), validTextRequest()); !errors.Is(err, ErrCostOverflow) {
		t.Fatalf("cost error=%v", err)
	}
	unknown := validTextRequest()
	unknown.ModelID = "model-unknown"
	if _, err := gateway.Generate(context.Background(), unknown); !errors.Is(err, ErrUnknownRoute) {
		t.Fatalf("route error=%v", err)
	}
	invalid := validTextRequest()
	invalid.Messages = nil
	if _, err := gateway.Generate(context.Background(), invalid); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("request error=%v", err)
	}
	if providerCalls != 1 {
		t.Fatalf("provider calls=%d", providerCalls)
	}
	if _, err := NewRegistry([]Route{{ProviderID: "provider-a", Provider: provider, Models: map[string]Price{}}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("registry error=%v", err)
	}
}

type recordingRecorder struct {
	mu      sync.Mutex
	records []CallRecord
}

func (r *recordingRecorder) Record(value CallRecord) {
	r.mu.Lock()
	r.records = append(r.records, value)
	r.mu.Unlock()
}
func (r *recordingRecorder) values() []CallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CallRecord(nil), r.records...)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) advance(value time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(value)
	c.mu.Unlock()
}

type fakeSleeper struct {
	delays []time.Duration
	hook   func()
}

func (s *fakeSleeper) Sleep(ctx context.Context, value time.Duration) error {
	s.delays = append(s.delays, value)
	if s.hook != nil {
		s.hook()
	}
	return ctx.Err()
}

type maximumRandom struct{}

func (maximumRandom) Int63n(maximum int64) int64 { return maximum - 1 }

func testGateway(t *testing.T, provider Provider, price Price, validator StructuredValidator, recorder Recorder, configuration Config) *Gateway {
	t.Helper()
	registry, err := NewRegistry([]Route{{ProviderID: "provider-a", Provider: provider, Models: map[string]Price{"model-v1": price}}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := New(registry, validator, recorder, configuration)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testGatewayConfig() Config {
	return Config{AttemptTimeout: time.Second, MaxAttempts: 3, RetryBase: time.Millisecond, RetryMaximum: 10 * time.Millisecond,
		FailureThreshold: 5, OpenDuration: time.Minute, Clock: &fakeClock{now: llmTestTime}, Sleeper: &fakeSleeper{}, Random: maximumRandom{}}
}

func validTextRequest() Request {
	return Request{ProviderID: "provider-a", ModelID: "model-v1", OperationKey: "op-0001",
		Messages: []Message{{Role: RoleSystem, Content: "policy"}, {Role: RoleUser, Content: "question"}}, MaxOutputTokens: 100}
}
func validStructuredRequest() Request {
	value := validTextRequest()
	value.ResponseSchema = json.RawMessage(`{"type":"object"}`)
	return value
}
func validTextResponse() ProviderResponse {
	return ProviderResponse{Text: "answer", FinishReason: FinishReasonStop, Usage: Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}
}
func acceptingValidator() StructuredValidator {
	return StructuredValidatorFunc(func(json.RawMessage, json.RawMessage) error { return nil })
}
func providerFailure(t *testing.T, kind ErrorKind, retryAfter time.Duration) error {
	t.Helper()
	value, err := NewProviderError(kind, retryAfter, errors.New("provider failure"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
