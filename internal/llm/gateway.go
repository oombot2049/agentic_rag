package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

type StructuredValidator interface {
	Validate(schema, value json.RawMessage) error
}

type StructuredValidatorFunc func(schema, value json.RawMessage) error

func (f StructuredValidatorFunc) Validate(schema, value json.RawMessage) error {
	return f(schema, value)
}

type Recorder interface{ Record(CallRecord) }

type RecorderFunc func(CallRecord)

func (f RecorderFunc) Record(record CallRecord) { f(record) }

type CallRecord struct {
	ProviderID   string
	ModelID      string
	OperationKey string
	Attempts     int
	StartedAt    time.Time
	Duration     time.Duration
	Succeeded    bool
	FailureKind  ErrorKind
	Usage        Usage
	CostNanos    int64
}

type Result struct {
	Response  ProviderResponse
	Attempts  int
	CostNanos int64
	Duration  time.Duration
}

type Clock interface{ Now() time.Time }
type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}
type Random interface{ Int63n(int64) int64 }

type Config struct {
	AttemptTimeout   time.Duration
	MaxAttempts      int
	RetryBase        time.Duration
	RetryMaximum     time.Duration
	FailureThreshold int
	OpenDuration     time.Duration
	Clock            Clock
	Sleeper          Sleeper
	Random           Random
}

func DefaultConfig() Config {
	return Config{AttemptTimeout: 30 * time.Second, MaxAttempts: 3, RetryBase: 100 * time.Millisecond,
		RetryMaximum: 2 * time.Second, FailureThreshold: 5, OpenDuration: 30 * time.Second}
}

type Gateway struct {
	registry  *Registry
	validator StructuredValidator
	recorder  Recorder
	config    Config
	clock     Clock
	sleeper   Sleeper
	random    Random

	mu       sync.Mutex
	breakers map[string]*breaker
}

func New(registry *Registry, validator StructuredValidator, recorder Recorder, configuration Config) (*Gateway, error) {
	if registry == nil || validator == nil || recorder == nil || validateConfig(configuration) != nil {
		return nil, fmt.Errorf("%w: dependencies or configuration", ErrInvalidArgument)
	}
	clock, sleeper, random := configuration.Clock, configuration.Sleeper, configuration.Random
	if clock == nil {
		clock = systemClock{}
	}
	if sleeper == nil {
		sleeper = systemSleeper{}
	}
	if random == nil {
		random = &lockedRandom{source: rand.New(rand.NewSource(time.Now().UnixNano()))}
	}
	return &Gateway{registry: registry, validator: validator, recorder: recorder, config: configuration,
		clock: clock, sleeper: sleeper, random: random, breakers: make(map[string]*breaker)}, nil
}

func validateConfig(value Config) error {
	if value.AttemptTimeout <= 0 || value.AttemptTimeout > 10*time.Minute || value.MaxAttempts < 1 || value.MaxAttempts > 5 ||
		value.RetryBase <= 0 || value.RetryMaximum < value.RetryBase || value.RetryMaximum > time.Minute ||
		value.FailureThreshold < 1 || value.FailureThreshold > 100 || value.OpenDuration <= 0 || value.OpenDuration > time.Hour {
		return ErrInvalidArgument
	}
	return nil
}

func (g *Gateway) Generate(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	route, err := g.registry.resolve(request.ProviderID, request.ModelID)
	if err != nil {
		return Result{}, err
	}
	providerBreaker := g.breaker(request.ProviderID)
	admission, err := providerBreaker.allow(g.clock.Now().UTC())
	if err != nil {
		g.record(request, g.clock.Now().UTC(), 0, Usage{}, 0, false, ErrorUnavailable)
		return Result{}, err
	}
	started := g.clock.Now().UTC()
	attempts := 0
	for {
		attempts++
		response, callErr := g.attempt(ctx, route.provider, request)
		if callErr == nil {
			callErr = g.validateResponse(request, response)
		}
		if callErr == nil {
			cost, costErr := calculateCost(response.Usage, route.price)
			if costErr != nil {
				providerBreaker.fail(g.clock.Now().UTC(), false, admission)
				g.record(request, started, attempts, response.Usage, 0, false, ErrorAdapter)
				return Result{}, costErr
			}
			providerBreaker.succeed()
			duration := nonNegativeDuration(started, g.clock.Now().UTC())
			g.recorder.Record(CallRecord{ProviderID: request.ProviderID, ModelID: request.ModelID,
				OperationKey: request.OperationKey, Attempts: attempts, StartedAt: started, Duration: duration,
				Succeeded: true, Usage: response.Usage, CostNanos: cost})
			return Result{Response: cloneResponse(response), Attempts: attempts, CostNanos: cost, Duration: duration}, nil
		}
		if ctx.Err() != nil {
			providerBreaker.abandon(admission)
			g.record(request, started, attempts, Usage{}, 0, false, ErrorAdapter)
			return Result{}, ctx.Err()
		}
		providerError := normalizeProviderError(callErr)
		if !providerError.Retryable() || attempts >= g.config.MaxAttempts {
			providerBreaker.fail(g.clock.Now().UTC(), providerError.circuitRelevant(), admission)
			g.record(request, started, attempts, Usage{}, 0, false, providerError.Kind())
			return Result{}, providerError
		}
		delay := g.retryDelay(attempts, providerError.RetryAfter())
		if err := g.sleeper.Sleep(ctx, delay); err != nil {
			providerBreaker.abandon(admission)
			g.record(request, started, attempts, Usage{}, 0, false, ErrorAdapter)
			return Result{}, err
		}
	}
}

func (g *Gateway) attempt(ctx context.Context, provider Provider, request Request) (ProviderResponse, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, g.config.AttemptTimeout)
	defer cancel()
	response, err := provider.Generate(attemptCtx, ProviderRequest{ModelID: request.ModelID, OperationKey: request.OperationKey,
		Messages: cloneMessages(request.Messages), MaxOutputTokens: request.MaxOutputTokens, ResponseSchema: cloneJSON(request.ResponseSchema)})
	if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		value, _ := NewProviderError(ErrorTimeout, 0, context.DeadlineExceeded)
		return ProviderResponse{}, value
	}
	return response, err
}

func (g *Gateway) validateResponse(request Request, response ProviderResponse) error {
	if !validFinishReason(response.FinishReason) || !validUsage(response.Usage) ||
		response.Usage.OutputTokens > int64(request.MaxOutputTokens) ||
		len(response.Text) > 4<<20 || len(response.Structured) > 4<<20 ||
		(response.ProviderRequestID != "" && !validIdentifier(response.ProviderRequestID, 256)) {
		return badResponse("finish reason, usage, or request ID")
	}
	if response.FinishReason == FinishReasonSafety {
		value, _ := NewProviderError(ErrorSafety, 0, errors.New("provider safety stop"))
		return value
	}
	if len(request.ResponseSchema) > 0 {
		if strings.TrimSpace(response.Text) != "" || !validSingleJSON(response.Structured) {
			return badResponse("structured response shape")
		}
		if err := g.validator.Validate(cloneJSON(request.ResponseSchema), cloneJSON(response.Structured)); err != nil {
			return badResponse("structured response contract")
		}
	} else if strings.TrimSpace(response.Text) == "" || len(response.Structured) != 0 {
		return badResponse("text response shape")
	}
	return nil
}

func validateRequest(value Request) error {
	if !validIdentifier(value.ProviderID, 128) || !validIdentifier(value.ModelID, 256) ||
		!validIdentifier(value.OperationKey, 256) || value.MaxOutputTokens < 1 || value.MaxOutputTokens > 1_000_000 ||
		len(value.Messages) == 0 || len(value.Messages) > 4_096 || len(value.ResponseSchema) > 1<<20 {
		return fmt.Errorf("%w: request identity or limits", ErrInvalidArgument)
	}
	totalContent := 0
	for _, message := range value.Messages {
		if !validRole(message.Role) || strings.TrimSpace(message.Content) == "" || len(message.Content) > 4<<20 {
			return fmt.Errorf("%w: message", ErrInvalidArgument)
		}
		totalContent += len(message.Content)
		if totalContent > 16<<20 {
			return fmt.Errorf("%w: total message content", ErrInvalidArgument)
		}
	}
	if len(value.ResponseSchema) > 0 && !validSingleJSON(value.ResponseSchema) {
		return fmt.Errorf("%w: response schema", ErrInvalidArgument)
	}
	return nil
}

func validRole(value Role) bool {
	return value == RoleSystem || value == RoleUser || value == RoleAssistant || value == RoleTool
}
func validFinishReason(value FinishReason) bool {
	return value == FinishReasonStop || value == FinishReasonLength || value == FinishReasonToolCall || value == FinishReasonSafety
}
func validUsage(value Usage) bool {
	return value.InputTokens >= 0 && value.OutputTokens >= 0 && value.TotalTokens >= 0 &&
		value.InputTokens <= 10_000_000 && value.OutputTokens <= 10_000_000 &&
		value.TotalTokens == value.InputTokens+value.OutputTokens
}

func validSingleJSON(value json.RawMessage) bool {
	if !json.Valid(value) {
		return false
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	if err := decoder.Decode(&decoded); err != nil {
		return false
	}
	return decoded != nil
}

func badResponse(reason string) error {
	value, _ := NewProviderError(ErrorBadResponse, 0, fmt.Errorf("%w: %s", ErrInvalidResponse, reason))
	return value
}

func normalizeProviderError(err error) *ProviderError {
	var value *ProviderError
	if errors.As(err, &value) && value != nil && value.kind.valid() {
		copyValue := *value
		return &copyValue
	}
	value, _ = NewProviderError(ErrorAdapter, 0, err)
	return value
}

func (g *Gateway) retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	maximum := g.config.RetryBase
	for current := 1; current < attempt && maximum < g.config.RetryMaximum; current++ {
		if maximum > g.config.RetryMaximum/2 {
			maximum = g.config.RetryMaximum
			break
		}
		maximum *= 2
	}
	if maximum > g.config.RetryMaximum {
		maximum = g.config.RetryMaximum
	}
	delay := time.Duration(g.random.Int63n(int64(maximum) + 1))
	if delay < 0 || delay > maximum {
		delay = 0
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > g.config.RetryMaximum {
		return g.config.RetryMaximum
	}
	return delay
}

func (g *Gateway) breaker(providerID string) *breaker {
	g.mu.Lock()
	defer g.mu.Unlock()
	value := g.breakers[providerID]
	if value == nil {
		value = newBreaker(g.config.FailureThreshold, g.config.OpenDuration)
		g.breakers[providerID] = value
	}
	return value
}

func (g *Gateway) BreakerSnapshot(providerID string) (BreakerSnapshot, bool) {
	g.mu.Lock()
	value := g.breakers[providerID]
	g.mu.Unlock()
	if value == nil {
		return BreakerSnapshot{}, false
	}
	return value.snapshot(), true
}

func (g *Gateway) record(request Request, started time.Time, attempts int, usage Usage, cost int64, succeeded bool, kind ErrorKind) {
	g.recorder.Record(CallRecord{ProviderID: request.ProviderID, ModelID: request.ModelID, OperationKey: request.OperationKey,
		Attempts: attempts, StartedAt: started, Duration: nonNegativeDuration(started, g.clock.Now().UTC()),
		Succeeded: succeeded, FailureKind: kind, Usage: usage, CostNanos: cost})
}

func cloneMessages(values []Message) []Message        { return append([]Message(nil), values...) }
func cloneJSON(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }
func cloneResponse(value ProviderResponse) ProviderResponse {
	value.Structured = cloneJSON(value.Structured)
	return value
}
func nonNegativeDuration(start, end time.Time) time.Duration {
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type systemSleeper struct{}

func (systemSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type lockedRandom struct {
	mu     sync.Mutex
	source *rand.Rand
}

func (r *lockedRandom) Int63n(maximum int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.source.Int63n(maximum)
}
