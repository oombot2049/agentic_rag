// Package tools defines the deny-by-default Agent tool protocol.
package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrInvalid        = errors.New("invalid tool protocol input")
	ErrDenied         = errors.New("tool invocation denied")
	ErrConflict       = errors.New("tool definition conflict")
	ErrSealed         = errors.New("tool registry is sealed")
	ErrInvalidPayload = errors.New("tool payload failed validation")
)

var (
	namePattern       = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_-]*)*$`)
	versionPattern    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	permissionPattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,127}$`)
)

const maximumJSONBytes = 64 << 10

type Effect string

const (
	EffectReadOnly Effect = "READ_ONLY"
)

type ScopeLevel string

const (
	ScopeTenant  ScopeLevel = "TENANT"
	ScopeSession ScopeLevel = "SESSION"
	ScopeRun     ScopeLevel = "RUN"
)

type Definition struct {
	Name                string
	Version             string
	Description         string
	InputSchema         json.RawMessage
	OutputSchema        json.RawMessage
	RequiredPermissions []string
	Scope               ScopeLevel
	Effect              Effect
}

// SchemaEngine implementations must be safe for concurrent Validate calls after Seal.
type SchemaEngine interface {
	Compile(json.RawMessage) error
	Validate(schema, payload json.RawMessage) error
}

type Scope struct {
	TenantID  domain.TenantID
	SessionID domain.SessionID
	RunID     domain.RunID
}

type Call struct {
	Name        string
	Version     string
	Scope       Scope
	Permissions []string
	Arguments   json.RawMessage
}

type Invocation struct {
	Definition    Definition
	Scope         Scope
	Arguments     json.RawMessage
	authorization [sha256.Size]byte
}

type Registry struct {
	mu          sync.RWMutex
	definitions map[string]Definition
	schemas     SchemaEngine
	sealed      bool
}

func NewRegistry(schemas SchemaEngine) (*Registry, error) {
	if schemas == nil {
		return nil, ErrInvalid
	}
	return &Registry{definitions: make(map[string]Definition), schemas: schemas}, nil
}

func (r *Registry) Register(value Definition) error {
	normalized, err := validateDefinition(value, r.schemas)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrSealed
	}
	key := toolKey(normalized.Name, normalized.Version)
	if _, exists := r.definitions[key]; exists {
		return ErrConflict
	}
	r.definitions[key] = normalized
	return nil
}

func (r *Registry) Seal() {
	r.mu.Lock()
	r.sealed = true
	r.mu.Unlock()
}

// Discover returns only definitions invocable in the exact scope. Empty does
// not distinguish an absent tool from one hidden by authorization.
func (r *Registry) Discover(scope Scope, permissions []string) []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.sealed {
		return nil
	}
	result := make([]Definition, 0, len(r.definitions))
	for _, definition := range r.definitions {
		if validScope(definition.Scope, scope) && hasPermissions(definition.RequiredPermissions, permissions) {
			result = append(result, cloneDefinition(definition))
		}
	}
	slices.SortFunc(result, func(left, right Definition) int {
		return strings.Compare(toolKey(left.Name, left.Version), toolKey(right.Name, right.Version))
	})
	return result
}

func (r *Registry) Resolve(ctx context.Context, call Call) (Invocation, error) {
	if ctx == nil || ctx.Err() != nil {
		return Invocation{}, ErrDenied
	}
	r.mu.RLock()
	definition, exists := r.definitions[toolKey(call.Name, call.Version)]
	sealed := r.sealed
	r.mu.RUnlock()
	if !sealed || !exists || !validScope(definition.Scope, call.Scope) ||
		!hasPermissions(definition.RequiredPermissions, call.Permissions) {
		return Invocation{}, ErrDenied
	}
	if err := strictJSONObject(call.Arguments); err != nil {
		return Invocation{}, fmt.Errorf("%w: arguments", ErrInvalidPayload)
	}
	if err := r.schemas.Validate(definition.InputSchema, call.Arguments); err != nil {
		return Invocation{}, fmt.Errorf("%w: arguments", ErrInvalidPayload)
	}
	invocation := Invocation{Definition: cloneDefinition(definition), Scope: call.Scope,
		Arguments: append(json.RawMessage(nil), call.Arguments...)}
	invocation.authorization = invocationDigest(invocation)
	return invocation, nil
}

// ValidateInvocation verifies that Resolve issued the capability and that no
// definition, scope or argument byte was changed afterwards.
func (r *Registry) ValidateInvocation(invocation Invocation) error {
	r.mu.RLock()
	definition, exists := r.definitions[toolKey(invocation.Definition.Name, invocation.Definition.Version)]
	sealed := r.sealed
	r.mu.RUnlock()
	if !sealed || !exists || definitionDigest(definition) != definitionDigest(invocation.Definition) ||
		invocation.authorization != invocationDigest(invocation) || !validScope(definition.Scope, invocation.Scope) {
		return ErrDenied
	}
	if err := strictJSONObject(invocation.Arguments); err != nil {
		return ErrDenied
	}
	if err := r.schemas.Validate(definition.InputSchema, invocation.Arguments); err != nil {
		return ErrDenied
	}
	return nil
}

func (r *Registry) ValidateOutput(invocation Invocation, output json.RawMessage) error {
	if err := r.ValidateInvocation(invocation); err != nil {
		return err
	}
	if err := strictJSONObject(output); err != nil {
		return fmt.Errorf("%w: output", ErrInvalidPayload)
	}
	r.mu.RLock()
	definition, exists := r.definitions[toolKey(invocation.Definition.Name, invocation.Definition.Version)]
	sealed := r.sealed
	r.mu.RUnlock()
	if !sealed || !exists || definitionDigest(definition) != definitionDigest(invocation.Definition) {
		return ErrDenied
	}
	if err := r.schemas.Validate(definition.OutputSchema, output); err != nil {
		return fmt.Errorf("%w: output", ErrInvalidPayload)
	}
	return nil
}

func validateDefinition(value Definition, schemas SchemaEngine) (Definition, error) {
	if !namePattern.MatchString(value.Name) || !versionPattern.MatchString(value.Version) ||
		len(value.Description) < 1 || len(value.Description) > 512 ||
		strings.TrimSpace(value.Description) != value.Description ||
		strings.IndexFunc(value.Description, unicode.IsControl) >= 0 ||
		!validEffect(value.Effect) || !validScopeLevel(value.Scope) ||
		len(value.RequiredPermissions) < 1 || len(value.RequiredPermissions) > 64 {
		return Definition{}, ErrInvalid
	}
	permissions := append([]string(nil), value.RequiredPermissions...)
	slices.Sort(permissions)
	for index, permission := range permissions {
		if !permissionPattern.MatchString(permission) || (index > 0 && permissions[index-1] == permission) {
			return Definition{}, ErrInvalid
		}
	}
	for _, schema := range []json.RawMessage{value.InputSchema, value.OutputSchema} {
		if err := strictJSONObject(schema); err != nil {
			return Definition{}, ErrInvalid
		}
		if err := schemas.Compile(schema); err != nil {
			return Definition{}, ErrInvalid
		}
	}
	value.RequiredPermissions = permissions
	value.InputSchema = append(json.RawMessage(nil), value.InputSchema...)
	value.OutputSchema = append(json.RawMessage(nil), value.OutputSchema...)
	return value, nil
}

func strictJSONObject(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || len(trimmed) > maximumJSONBytes || !json.Valid(trimmed) || trimmed[0] != '{' {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if err = walkObject(decoder, token); err != nil {
		return err
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

func walkObject(decoder *json.Decoder, token json.Token) error {
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return ErrInvalid
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return ErrInvalid
		}
		if _, exists := seen[key]; exists {
			return ErrInvalid
		}
		seen[key] = struct{}{}
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if err = walkContainer(decoder, value); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return ErrInvalid
	}
	return nil
}

func walkArray(decoder *json.Decoder) error {
	for decoder.More() {
		value, err := decoder.Token()
		if err != nil {
			return err
		}
		if err = walkContainer(decoder, value); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim(']') {
		return ErrInvalid
	}
	return nil
}

func walkContainer(decoder *json.Decoder, value json.Token) error {
	child, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	switch child {
	case '{':
		return walkObject(decoder, child)
	case '[':
		return walkArray(decoder)
	default:
		return ErrInvalid
	}
}

func validScope(level ScopeLevel, value Scope) bool {
	if _, err := domain.ParseTenantID(value.TenantID.String()); err != nil {
		return false
	}
	switch level {
	case ScopeTenant:
		return value.SessionID == "" && value.RunID == ""
	case ScopeSession:
		if _, err := domain.ParseSessionID(value.SessionID.String()); err != nil {
			return false
		}
		return value.RunID == ""
	case ScopeRun:
		if _, err := domain.ParseSessionID(value.SessionID.String()); err != nil {
			return false
		}
		_, err := domain.ParseRunID(value.RunID.String())
		return err == nil
	default:
		return false
	}
}

func hasPermissions(required, actual []string) bool {
	set := make(map[string]struct{}, len(actual))
	for _, permission := range actual {
		if !permissionPattern.MatchString(permission) {
			return false
		}
		set[permission] = struct{}{}
	}
	for _, permission := range required {
		if _, exists := set[permission]; !exists {
			return false
		}
	}
	return true
}

func validEffect(value Effect) bool {
	return value == EffectReadOnly
}
func validScopeLevel(value ScopeLevel) bool {
	return value == ScopeTenant || value == ScopeSession || value == ScopeRun
}
func toolKey(name, version string) string { return name + "@" + version }
func cloneDefinition(value Definition) Definition {
	value.InputSchema = append(json.RawMessage(nil), value.InputSchema...)
	value.OutputSchema = append(json.RawMessage(nil), value.OutputSchema...)
	value.RequiredPermissions = append([]string(nil), value.RequiredPermissions...)
	return value
}
func definitionDigest(value Definition) string {
	return value.Name + "\x00" + value.Version + "\x00" + value.Description + "\x00" +
		string(value.Scope) + "\x00" + string(value.Effect) + "\x00" +
		string(value.InputSchema) + "\x00" + string(value.OutputSchema) + "\x00" +
		strings.Join(value.RequiredPermissions, "\x00")
}

func invocationDigest(value Invocation) [sha256.Size]byte {
	material := definitionDigest(value.Definition) + "\x00" + value.Scope.TenantID.String() + "\x00" +
		value.Scope.SessionID.String() + "\x00" + value.Scope.RunID.String() + "\x00" + string(value.Arguments)
	return sha256.Sum256([]byte(material))
}
