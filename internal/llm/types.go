// Package llm provides provider-neutral model invocation policy.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

var (
	ErrInvalidArgument = errors.New("llm invalid argument")
	ErrUnknownRoute    = errors.New("llm unknown provider or model")
	ErrCircuitOpen     = errors.New("llm provider circuit open")
	ErrInvalidResponse = errors.New("llm invalid provider response")
	ErrCostOverflow    = errors.New("llm cost overflow")
)

type Role string

const (
	RoleSystem    Role = "SYSTEM"
	RoleUser      Role = "USER"
	RoleAssistant Role = "ASSISTANT"
	RoleTool      Role = "TOOL"
)

type Message struct {
	Role    Role
	Content string
}

type Request struct {
	ProviderID      string
	ModelID         string
	OperationKey    string
	Messages        []Message
	MaxOutputTokens int
	ResponseSchema  json.RawMessage
}

type ProviderRequest struct {
	ModelID         string
	OperationKey    string
	Messages        []Message
	MaxOutputTokens int
	ResponseSchema  json.RawMessage
}

type FinishReason string

const (
	FinishReasonStop     FinishReason = "STOP"
	FinishReasonLength   FinishReason = "LENGTH"
	FinishReasonToolCall FinishReason = "TOOL_CALL"
	FinishReasonSafety   FinishReason = "SAFETY"
)

type Usage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

type ProviderResponse struct {
	Text              string
	Structured        json.RawMessage
	FinishReason      FinishReason
	Usage             Usage
	ProviderRequestID string
}

type Provider interface {
	Generate(context.Context, ProviderRequest) (ProviderResponse, error)
}

type ProviderFunc func(context.Context, ProviderRequest) (ProviderResponse, error)

func (f ProviderFunc) Generate(ctx context.Context, request ProviderRequest) (ProviderResponse, error) {
	return f(ctx, request)
}

type ErrorKind string

const (
	ErrorInvalidRequest ErrorKind = "INVALID_REQUEST"
	ErrorAuthentication ErrorKind = "AUTHENTICATION"
	ErrorRateLimited    ErrorKind = "RATE_LIMITED"
	ErrorTimeout        ErrorKind = "TIMEOUT"
	ErrorUnavailable    ErrorKind = "UNAVAILABLE"
	ErrorSafety         ErrorKind = "SAFETY"
	ErrorBadResponse    ErrorKind = "BAD_RESPONSE"
	ErrorAdapter        ErrorKind = "ADAPTER_ERROR"
)

type ProviderError struct {
	kind       ErrorKind
	retryAfter time.Duration
	cause      error
}

func NewProviderError(kind ErrorKind, retryAfter time.Duration, cause error) (*ProviderError, error) {
	if !kind.valid() || retryAfter < 0 || retryAfter > 24*time.Hour || (kind != ErrorRateLimited && retryAfter != 0) {
		return nil, fmt.Errorf("%w: provider error", ErrInvalidArgument)
	}
	return &ProviderError{kind: kind, retryAfter: retryAfter, cause: cause}, nil
}

func (e *ProviderError) Error() string {
	if e == nil {
		return string(ErrorAdapter)
	}
	return string(e.kind)
}
func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
func (e *ProviderError) Kind() ErrorKind {
	if e == nil || !e.kind.valid() {
		return ErrorAdapter
	}
	return e.kind
}
func (e *ProviderError) RetryAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.retryAfter
}
func (e *ProviderError) Retryable() bool {
	return e != nil && (e.kind == ErrorRateLimited || e.kind == ErrorTimeout || e.kind == ErrorUnavailable)
}
func (e *ProviderError) circuitRelevant() bool {
	return e != nil && (e.Retryable() || e.kind == ErrorBadResponse)
}

func (k ErrorKind) valid() bool {
	switch k {
	case ErrorInvalidRequest, ErrorAuthentication, ErrorRateLimited, ErrorTimeout,
		ErrorUnavailable, ErrorSafety, ErrorBadResponse, ErrorAdapter:
		return true
	default:
		return false
	}
}

type Price struct {
	InputNanosPerMillionTokens  int64
	OutputNanosPerMillionTokens int64
}

type Route struct {
	ProviderID string
	Provider   Provider
	Models     map[string]Price
}

type route struct {
	provider Provider
	price    Price
}

type Registry struct {
	routes    map[string]route
	providers map[string]struct{}
}

func NewRegistry(routes []Route) (*Registry, error) {
	result := &Registry{routes: make(map[string]route), providers: make(map[string]struct{})}
	for _, configured := range routes {
		if !validIdentifier(configured.ProviderID, 128) || configured.Provider == nil || len(configured.Models) == 0 {
			return nil, fmt.Errorf("%w: provider route", ErrInvalidArgument)
		}
		if _, duplicate := result.providers[configured.ProviderID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate provider", ErrInvalidArgument)
		}
		result.providers[configured.ProviderID] = struct{}{}
		for modelID, price := range configured.Models {
			if !validIdentifier(modelID, 256) || price.InputNanosPerMillionTokens < 0 || price.OutputNanosPerMillionTokens < 0 {
				return nil, fmt.Errorf("%w: model route", ErrInvalidArgument)
			}
			result.routes[routeKey(configured.ProviderID, modelID)] = route{provider: configured.Provider, price: price}
		}
	}
	if len(result.routes) == 0 {
		return nil, fmt.Errorf("%w: empty registry", ErrInvalidArgument)
	}
	return result, nil
}

func (r *Registry) resolve(providerID, modelID string) (route, error) {
	if r == nil {
		return route{}, ErrUnknownRoute
	}
	value, ok := r.routes[routeKey(providerID, modelID)]
	if !ok {
		return route{}, fmt.Errorf("%w: %s/%s", ErrUnknownRoute, providerID, modelID)
	}
	return value, nil
}

func routeKey(providerID, modelID string) string { return providerID + "\x00" + modelID }

func calculateCost(usage Usage, price Price) (int64, error) {
	input, err := roundedCost(usage.InputTokens, price.InputNanosPerMillionTokens)
	if err != nil {
		return 0, err
	}
	output, err := roundedCost(usage.OutputTokens, price.OutputNanosPerMillionTokens)
	if err != nil || input > math.MaxInt64-output {
		return 0, ErrCostOverflow
	}
	return input + output, nil
}

func roundedCost(tokens, rate int64) (int64, error) {
	if tokens < 0 || rate < 0 || (tokens != 0 && rate > math.MaxInt64/tokens) {
		return 0, ErrCostOverflow
	}
	product := tokens * rate
	if product == 0 {
		return 0, nil
	}
	return 1 + (product-1)/1_000_000, nil
}

func validIdentifier(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}
