// Package executor enforces bounded, deny-by-default Tool execution.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
	"github.com/agenticrag/agenticrag/internal/tools"
)

var (
	ErrInvalid       = errors.New("invalid tool execution configuration")
	ErrDenied        = errors.New("tool execution denied")
	ErrCapacity      = errors.New("tool execution capacity unavailable")
	ErrTimeout       = errors.New("tool execution timed out")
	ErrCanceled      = errors.New("tool execution canceled")
	ErrOutputLimit   = errors.New("tool output exceeds limit")
	ErrInvalidOutput = errors.New("tool output rejected")
	ErrAdapter       = errors.New("tool adapter failed")
)

const (
	MaximumTimeout     = 2 * time.Minute
	MaximumOutputBytes = 64 << 10
	MaximumConcurrency = 1024
)

type NetworkAccess string

const (
	NetworkNone     NetworkAccess = "NONE"
	NetworkBrokered NetworkAccess = "BROKERED"
)

type Capabilities struct {
	Network         NetworkAccess
	Destinations    []string
	IsolatedCommand bool
}

type AdapterRequest struct {
	TenantID      domain.TenantID
	Name          string
	Version       string
	Scope         tools.Scope
	Arguments     json.RawMessage
	InvocationKey string
}

type Adapter interface {
	Name() string
	Version() string
	Capabilities() Capabilities
	Execute(context.Context, AdapterRequest) (io.ReadCloser, error)
}

type Policy struct {
	Enabled              bool
	Timeout              time.Duration
	MaximumOutputBytes   int64
	TenantConcurrency    int
	ToolConcurrency      int
	AllowBrokeredNetwork bool
	AllowedHosts         []string
	AllowIsolatedCommand bool
}

type PolicyResolver interface {
	Resolve(context.Context, domain.TenantID, string, string) (Policy, error)
}

type Request struct {
	Invocation    tools.Invocation
	InvocationKey string
}

type Result struct{ Output json.RawMessage }

type Executor struct {
	registry *tools.Registry
	policies PolicyResolver
	adapters map[string]Adapter
	limits   *hierarchicalLimiter
}

func New(registry *tools.Registry, policies PolicyResolver, adapters []Adapter, globalConcurrency int) (*Executor, error) {
	if registry == nil || policies == nil || globalConcurrency < 1 || globalConcurrency > MaximumConcurrency {
		return nil, ErrInvalid
	}
	values := make(map[string]Adapter, len(adapters))
	for _, adapter := range adapters {
		if adapter == nil || !validIdentity(adapter.Name(), adapter.Version()) {
			return nil, ErrInvalid
		}
		key := adapter.Name() + "@" + adapter.Version()
		if _, exists := values[key]; exists {
			return nil, ErrInvalid
		}
		if _, err := normalizeCapabilities(adapter.Capabilities()); err != nil {
			return nil, ErrInvalid
		}
		values[key] = adapter
	}
	return &Executor{registry: registry, policies: policies, adapters: values, limits: newHierarchicalLimiter(globalConcurrency)}, nil
}

func (e *Executor) Execute(ctx context.Context, request Request) (Result, error) {
	if ctx == nil || ctx.Err() != nil || request.InvocationKey == "" {
		return Result{}, ErrInvalid
	}
	definition := request.Invocation.Definition
	if err := e.registry.ValidateInvocation(request.Invocation); err != nil {
		return Result{}, ErrDenied
	}
	adapter, exists := e.adapters[definition.Name+"@"+definition.Version]
	if !exists {
		return Result{}, ErrDenied
	}
	policy, err := e.policies.Resolve(ctx, request.Invocation.Scope.TenantID, definition.Name, definition.Version)
	if err != nil || !policy.Enabled || validatePolicy(policy) != nil {
		return Result{}, ErrDenied
	}
	capabilities, err := normalizeCapabilities(adapter.Capabilities())
	if err != nil || !capabilitiesAllowed(capabilities, policy) {
		return Result{}, ErrDenied
	}

	executionCtx, cancel := context.WithTimeout(ctx, policy.Timeout)
	defer cancel()
	toolKey := definition.Name + "@" + definition.Version
	release, err := e.limits.acquire(executionCtx, request.Invocation.Scope.TenantID.String(), toolKey,
		policy.TenantConcurrency, policy.ToolConcurrency)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ErrCanceled
		}
		return Result{}, ErrCapacity
	}

	completed := make(chan executionResult, 1)
	go func() {
		defer release()
		completed <- e.run(executionCtx, adapter, request, policy.MaximumOutputBytes)
	}()
	select {
	case value := <-completed:
		if executionCtx.Err() != nil {
			if ctx.Err() != nil {
				return Result{}, ErrCanceled
			}
			return Result{}, ErrTimeout
		}
		return value.result, value.err
	case <-executionCtx.Done():
		if ctx.Err() != nil {
			return Result{}, ErrCanceled
		}
		return Result{}, ErrTimeout
	}
}

type executionResult struct {
	result Result
	err    error
}

func (e *Executor) run(ctx context.Context, adapter Adapter, request Request, maximum int64) executionResult {
	invocation := request.Invocation
	adapterRequest := AdapterRequest{TenantID: invocation.Scope.TenantID, Name: invocation.Definition.Name,
		Version: invocation.Definition.Version, Scope: invocation.Scope,
		Arguments: append(json.RawMessage(nil), invocation.Arguments...), InvocationKey: request.InvocationKey}
	reader, err := adapter.Execute(ctx, adapterRequest)
	if err != nil || reader == nil {
		return executionResult{err: ErrAdapter}
	}
	defer reader.Close()
	output, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return executionResult{err: ErrAdapter}
	}
	if int64(len(output)) > maximum {
		return executionResult{err: ErrOutputLimit}
	}
	if err := e.registry.ValidateOutput(invocation, json.RawMessage(output)); err != nil {
		return executionResult{err: ErrInvalidOutput}
	}
	return executionResult{result: Result{Output: append(json.RawMessage(nil), output...)}}
}

func validatePolicy(value Policy) error {
	if value.Timeout <= 0 || value.Timeout > MaximumTimeout || value.MaximumOutputBytes < 1 ||
		value.MaximumOutputBytes > MaximumOutputBytes || value.TenantConcurrency < 1 ||
		value.TenantConcurrency > MaximumConcurrency || value.ToolConcurrency < 1 ||
		value.ToolConcurrency > value.TenantConcurrency || len(value.AllowedHosts) > 64 {
		return ErrInvalid
	}
	_, err := normalizeHosts(value.AllowedHosts)
	return err
}

func normalizeCapabilities(value Capabilities) (Capabilities, error) {
	if value.Network != NetworkNone && value.Network != NetworkBrokered {
		return Capabilities{}, ErrInvalid
	}
	hosts, err := normalizeHosts(value.Destinations)
	if err != nil {
		return Capabilities{}, err
	}
	if value.Network == NetworkNone && len(hosts) != 0 {
		return Capabilities{}, ErrInvalid
	}
	value.Destinations = hosts
	return value, nil
}

func capabilitiesAllowed(capabilities Capabilities, policy Policy) bool {
	if capabilities.IsolatedCommand && !policy.AllowIsolatedCommand {
		return false
	}
	if capabilities.Network == NetworkNone {
		return true
	}
	if !policy.AllowBrokeredNetwork {
		return false
	}
	allowed, err := normalizeHosts(policy.AllowedHosts)
	if err != nil {
		return false
	}
	for _, host := range capabilities.Destinations {
		if !slices.Contains(allowed, host) {
			return false
		}
	}
	return len(capabilities.Destinations) > 0
}

var dnsName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

func normalizeHosts(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, raw := range values {
		host := strings.ToLower(raw)
		if host != raw || len(host) > 253 || !dnsName.MatchString(host) || net.ParseIP(host) != nil ||
			host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return nil, ErrInvalid
		}
		result = append(result, host)
	}
	slices.Sort(result)
	if len(slices.Compact(result)) != len(result) {
		return nil, ErrInvalid
	}
	return result, nil
}

func validIdentity(name, version string) bool {
	return regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_-]*)*$`).MatchString(name) &&
		regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version)
}

type hierarchicalLimiter struct {
	mu          sync.Mutex
	global      int
	globalLimit int
	tenants     map[string]int
	tools       map[string]int
	notify      chan struct{}
}

func newHierarchicalLimiter(globalLimit int) *hierarchicalLimiter {
	return &hierarchicalLimiter{globalLimit: globalLimit, tenants: make(map[string]int), tools: make(map[string]int), notify: make(chan struct{})}
}

func (l *hierarchicalLimiter) acquire(ctx context.Context, tenant, tool string, tenantLimit, toolLimit int) (func(), error) {
	key := tenant + "\x00" + tool
	for {
		l.mu.Lock()
		if l.global < l.globalLimit && l.tenants[tenant] < tenantLimit && l.tools[key] < toolLimit {
			l.global++
			l.tenants[tenant]++
			l.tools[key]++
			l.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { l.release(tenant, key) }) }, nil
		}
		notify := l.notify
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-notify:
		}
	}
}

func (l *hierarchicalLimiter) release(tenant, key string) {
	l.mu.Lock()
	l.global--
	l.tenants[tenant]--
	l.tools[key]--
	if l.tenants[tenant] == 0 {
		delete(l.tenants, tenant)
	}
	if l.tools[key] == 0 {
		delete(l.tools, key)
	}
	close(l.notify)
	l.notify = make(chan struct{})
	l.mu.Unlock()
}

func (l *hierarchicalLimiter) inUse() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.global
}
