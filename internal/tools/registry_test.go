package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/agenticrag/agenticrag/internal/domain"
)

type contractSchemas struct{}

func (contractSchemas) Compile(schema json.RawMessage) error {
	var value map[string]any
	if err := json.Unmarshal(schema, &value); err != nil || value["type"] != "object" {
		return errors.New("schema root must be object")
	}
	return nil
}

func (contractSchemas) Validate(schema, payload json.RawMessage) error {
	var specification struct {
		Required []string `json:"required"`
	}
	var value map[string]any
	if json.Unmarshal(schema, &specification) != nil || json.Unmarshal(payload, &value) != nil {
		return errors.New("invalid JSON")
	}
	for _, key := range specification.Required {
		if _, exists := value[key]; !exists {
			return fmt.Errorf("missing %s", key)
		}
	}
	if rejected, _ := value["reject"].(bool); rejected {
		return errors.New("rejected by test engine")
	}
	return nil
}

func TestRegistryContract(t *testing.T) {
	registry := newTestRegistry(t)
	definitions := []Definition{
		definition("knowledge.search", "1.0.0", ScopeTenant, EffectReadOnly, "knowledge.read"),
		definition("session.note", "2.1.0", ScopeSession, EffectIdempotentWrite, "session.write"),
		definition("run.cancel", "1.0.1", ScopeRun, EffectDestructive, "run.cancel"),
	}
	for _, value := range definitions {
		if err := registry.Register(value); err != nil {
			t.Fatalf("register %s: %v", value.Name, err)
		}
	}
	if err := registry.Register(definitions[0]); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}

	tenantScope := Scope{TenantID: domain.TenantID("ten_0001")}
	if got := registry.Discover(tenantScope, []string{"knowledge.read"}); got != nil {
		t.Fatalf("unsealed discover = %#v", got)
	}
	if _, err := registry.Resolve(context.Background(), callFor(definitions[0], tenantScope)); !errors.Is(err, ErrDenied) {
		t.Fatalf("unsealed resolve: %v", err)
	}

	registry.Seal()
	if err := registry.Register(definition("late.tool", "1.0.0", ScopeTenant, EffectReadOnly, "late.read")); !errors.Is(err, ErrSealed) {
		t.Fatalf("late registration: %v", err)
	}

	tests := []struct {
		definition Definition
		scope      Scope
	}{
		{definitions[0], tenantScope},
		{definitions[1], Scope{TenantID: domain.TenantID("ten_0001"), SessionID: domain.SessionID("ses_0001")}},
		{definitions[2], Scope{TenantID: domain.TenantID("ten_0001"), SessionID: domain.SessionID("ses_0001"), RunID: domain.RunID("run_0001")}},
	}
	for _, test := range tests {
		t.Run(test.definition.Name, func(t *testing.T) {
			invocation, err := registry.Resolve(context.Background(), callFor(test.definition, test.scope))
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if invocation.Definition.Effect != test.definition.Effect {
				t.Fatalf("effect = %s", invocation.Definition.Effect)
			}
			if err := registry.ValidateOutput(invocation, json.RawMessage(`{"answer":"ok"}`)); err != nil {
				t.Fatalf("validate output: %v", err)
			}
		})
	}

	visible := registry.Discover(tenantScope, []string{"knowledge.read", "session.write", "run.cancel"})
	if len(visible) != 1 || visible[0].Name != "knowledge.search" {
		t.Fatalf("visible = %#v", visible)
	}
}

func TestRegistryDeniesWithoutEnumeration(t *testing.T) {
	registry := newTestRegistry(t)
	value := definition("knowledge.search", "1.0.0", ScopeTenant, EffectReadOnly, "knowledge.read")
	if err := registry.Register(value); err != nil {
		t.Fatal(err)
	}
	registry.Seal()
	base := callFor(value, Scope{TenantID: domain.TenantID("ten_0001")})

	tests := []struct {
		name   string
		mutate func(*Call)
	}{
		{"unknown name", func(call *Call) { call.Name = "knowledge.missing" }},
		{"unknown version", func(call *Call) { call.Version = "9.0.0" }},
		{"missing permission", func(call *Call) { call.Permissions = nil }},
		{"malformed permission", func(call *Call) { call.Permissions = []string{"Knowledge.Read"} }},
		{"invalid tenant", func(call *Call) { call.Scope.TenantID = "other" }},
		{"ambiguous scope", func(call *Call) { call.Scope.SessionID = "ses_0001" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call := base
			test.mutate(&call)
			if _, err := registry.Resolve(context.Background(), call); !errors.Is(err, ErrDenied) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.Resolve(canceled, base); !errors.Is(err, ErrDenied) {
		t.Fatalf("canceled: %v", err)
	}
	if got := registry.Discover(base.Scope, nil); len(got) != 0 {
		t.Fatalf("unauthorized discovery = %#v", got)
	}
}

func TestRegistryRejectsInvalidDefinitions(t *testing.T) {
	valid := definition("knowledge.search", "1.0.0", ScopeTenant, EffectReadOnly, "knowledge.read")
	tests := []struct {
		name   string
		mutate func(*Definition)
	}{
		{"name", func(value *Definition) { value.Name = "Knowledge.Search" }},
		{"version", func(value *Definition) { value.Version = "latest" }},
		{"description", func(value *Definition) { value.Description = " bad" }},
		{"effect", func(value *Definition) { value.Effect = "WRITE" }},
		{"scope", func(value *Definition) { value.Scope = "GLOBAL" }},
		{"permissions empty", func(value *Definition) { value.RequiredPermissions = nil }},
		{"permissions duplicate", func(value *Definition) { value.RequiredPermissions = []string{"knowledge.read", "knowledge.read"} }},
		{"permission malformed", func(value *Definition) { value.RequiredPermissions = []string{"Knowledge.Read"} }},
		{"schema array", func(value *Definition) { value.InputSchema = json.RawMessage(`[]`) }},
		{"schema compile", func(value *Definition) { value.OutputSchema = json.RawMessage(`{"type":"string"}`) }},
		{"schema duplicate key", func(value *Definition) { value.InputSchema = json.RawMessage(`{"type":"object","type":"object"}`) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := newTestRegistry(t)
			value := valid
			test.mutate(&value)
			if err := registry.Register(value); !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := NewRegistry(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil schema engine: %v", err)
	}
}

func TestRegistryPayloadAndOutputValidation(t *testing.T) {
	registry := newTestRegistry(t)
	value := definition("knowledge.search", "1.0.0", ScopeTenant, EffectReadOnly, "knowledge.read")
	if err := registry.Register(value); err != nil {
		t.Fatal(err)
	}
	registry.Seal()
	base := callFor(value, Scope{TenantID: domain.TenantID("ten_0001")})
	for _, arguments := range []json.RawMessage{
		json.RawMessage(`{"other":1}`),
		json.RawMessage(`{"query":"ok","nested":{"a":1,"a":2}}`),
		json.RawMessage(`{"query":"ok","reject":true}`),
		json.RawMessage(`[]`),
	} {
		call := base
		call.Arguments = arguments
		if _, err := registry.Resolve(context.Background(), call); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("arguments %s: %v", arguments, err)
		}
	}

	invocation, err := registry.Resolve(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{"answer":"ok","reject":true}`), json.RawMessage(`null`)} {
		if err := registry.ValidateOutput(invocation, output); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("output %s: %v", output, err)
		}
	}
	invocation.Definition.Description = "tampered"
	if err := registry.ValidateOutput(invocation, json.RawMessage(`{"answer":"ok"}`)); !errors.Is(err, ErrDenied) {
		t.Fatalf("tampered definition: %v", err)
	}
}

func TestRegistryDefensiveCopiesAndConcurrentReads(t *testing.T) {
	registry := newTestRegistry(t)
	value := definition("knowledge.search", "1.0.0", ScopeTenant, EffectReadOnly, "knowledge.read")
	if err := registry.Register(value); err != nil {
		t.Fatal(err)
	}
	value.InputSchema[0] = '['
	value.RequiredPermissions[0] = "changed"
	registry.Seal()
	scope := Scope{TenantID: domain.TenantID("ten_0001")}
	call := callFor(definition("knowledge.search", "1.0.0", ScopeTenant, EffectReadOnly, "knowledge.read"), scope)

	var wait sync.WaitGroup
	errorsFound := make(chan error, 64)
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			invocation, err := registry.Resolve(context.Background(), call)
			if err != nil {
				errorsFound <- err
				return
			}
			invocation.Definition.InputSchema[0] = '['
			invocation.Definition.RequiredPermissions[0] = "changed"
			visible := registry.Discover(scope, []string{"knowledge.read"})
			if len(visible) != 1 {
				errorsFound <- fmt.Errorf("visible count %d", len(visible))
				return
			}
			visible[0].OutputSchema[0] = '['
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	visible := registry.Discover(scope, []string{"knowledge.read"})
	if len(visible) != 1 || !json.Valid(visible[0].InputSchema) || !slices.Equal(visible[0].RequiredPermissions, []string{"knowledge.read"}) {
		t.Fatalf("registry mutated: %#v", visible)
	}
}

func definition(name, version string, scope ScopeLevel, effect Effect, permission string) Definition {
	return Definition{
		Name: name, Version: version, Description: "Contract test tool",
		InputSchema:         json.RawMessage(`{"type":"object","required":["query"]}`),
		OutputSchema:        json.RawMessage(`{"type":"object","required":["answer"]}`),
		RequiredPermissions: []string{permission}, Scope: scope, Effect: effect,
	}
}

func callFor(value Definition, scope Scope) Call {
	return Call{Name: value.Name, Version: value.Version, Scope: scope,
		Permissions: append([]string(nil), value.RequiredPermissions...), Arguments: json.RawMessage(`{"query":"hello"}`)}
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := NewRegistry(contractSchemas{})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
