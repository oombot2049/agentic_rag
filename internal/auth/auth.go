// Package auth authenticates requests and carries trusted tenant Principals.
package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var (
	ErrInvalidPrincipal    = errors.New("invalid authenticated principal")
	ErrMissingCredential   = errors.New("authentication credential is missing")
	ErrMalformedCredential = errors.New("authentication credential is malformed")
	ErrInvalidCredential   = errors.New("authentication credential is invalid")
	ErrVerifierUnavailable = errors.New("authentication verifier is unavailable")
	ErrCredentialNotFound  = errors.New("credential record not found")
	permissionPattern      = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,127}$`)
)

type Principal struct {
	tenantID    domain.TenantID
	subjectID   string
	permissions map[string]struct{}
}

func NewPrincipal(tenantID domain.TenantID, subjectID string, permissions []string) (Principal, error) {
	parsedTenant, err := domain.ParseTenantID(tenantID.String())
	if err != nil || !validSubject(subjectID) {
		return Principal{}, ErrInvalidPrincipal
	}
	permissionSet := make(map[string]struct{}, len(permissions))
	for _, permission := range permissions {
		if !permissionPattern.MatchString(permission) {
			return Principal{}, ErrInvalidPrincipal
		}
		permissionSet[permission] = struct{}{}
	}
	return Principal{tenantID: parsedTenant, subjectID: subjectID, permissions: permissionSet}, nil
}

func (p Principal) TenantID() domain.TenantID { return p.tenantID }
func (p Principal) SubjectID() string         { return p.subjectID }
func (p Principal) HasPermission(permission string) bool {
	_, ok := p.permissions[permission]
	return ok
}
func (p Principal) Permissions() []string {
	permissions := make([]string, 0, len(p.permissions))
	for permission := range p.permissions {
		permissions = append(permissions, permission)
	}
	sort.Strings(permissions)
	return permissions
}

type CredentialKind string

const (
	CredentialBearer CredentialKind = "BEARER"
	CredentialAPIKey CredentialKind = "API_KEY"
)

type Credential struct {
	Kind  CredentialKind
	Value string
}

type Verifier interface {
	Verify(context.Context, string) (Principal, error)
}

type VerifierFunc func(context.Context, string) (Principal, error)

func (f VerifierFunc) Verify(ctx context.Context, value string) (Principal, error) {
	return f(ctx, value)
}

type Authenticator struct {
	verifiers map[CredentialKind]Verifier
}

func NewAuthenticator(bearer, apiKey Verifier) (*Authenticator, error) {
	verifiers := make(map[CredentialKind]Verifier, 2)
	if !isNil(bearer) {
		verifiers[CredentialBearer] = bearer
	}
	if !isNil(apiKey) {
		verifiers[CredentialAPIKey] = apiKey
	}
	if len(verifiers) == 0 {
		return nil, errors.New("at least one authentication verifier is required")
	}
	return &Authenticator{verifiers: verifiers}, nil
}

func (a *Authenticator) Authenticate(ctx context.Context, credential Credential) (Principal, error) {
	if a == nil {
		return Principal{}, ErrVerifierUnavailable
	}
	verifier, ok := a.verifiers[credential.Kind]
	if !ok {
		return Principal{}, ErrInvalidCredential
	}
	principal, err := verifier.Verify(ctx, credential.Value)
	if err != nil {
		return Principal{}, err
	}
	if err := validatePrincipal(principal); err != nil {
		return Principal{}, fmt.Errorf("verifier returned an invalid principal: %w", err)
	}
	return clonePrincipal(principal), nil
}

type APIKeyStore interface {
	ResolveAPIKey(context.Context, [sha256.Size]byte) (APIKeyRecord, error)
}

type APIKeyRecord struct {
	Principal Principal
	ExpiresAt time.Time
	Revoked   bool
}

type APIKeyVerifier struct {
	store APIKeyStore
	now   func() time.Time
}

func NewAPIKeyVerifier(store APIKeyStore) (*APIKeyVerifier, error) {
	if isNil(store) {
		return nil, errors.New("API key store is required")
	}
	return &APIKeyVerifier{store: store, now: time.Now}, nil
}

func (v *APIKeyVerifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if !validSecret(raw, 16, 4096) {
		return Principal{}, ErrInvalidCredential
	}
	digest := sha256.Sum256([]byte(raw))
	record, err := v.store.ResolveAPIKey(ctx, digest)
	if errors.Is(err, ErrCredentialNotFound) {
		return Principal{}, ErrInvalidCredential
	}
	if err != nil {
		return Principal{}, errors.Join(ErrVerifierUnavailable, fmt.Errorf("resolve API key: %w", err))
	}
	if record.Revoked || record.ExpiresAt.IsZero() || !record.ExpiresAt.After(v.now().UTC()) {
		return Principal{}, ErrInvalidCredential
	}
	if err := validatePrincipal(record.Principal); err != nil {
		return Principal{}, fmt.Errorf("%w: corrupt API key principal", ErrVerifierUnavailable)
	}
	return clonePrincipal(record.Principal), nil
}

type principalContextKey struct{}

func withPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, clonePrincipal(principal))
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	if !ok || validatePrincipal(principal) != nil {
		return Principal{}, false
	}
	return clonePrincipal(principal), true
}

func TenantID(ctx context.Context) (domain.TenantID, bool) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return "", false
	}
	return principal.TenantID(), true
}

func validatePrincipal(principal Principal) error {
	_, err := NewPrincipal(principal.tenantID, principal.subjectID, principal.Permissions())
	return err
}

func clonePrincipal(principal Principal) Principal {
	permissions := make(map[string]struct{}, len(principal.permissions))
	for permission := range principal.permissions {
		permissions[permission] = struct{}{}
	}
	return Principal{tenantID: principal.tenantID, subjectID: principal.subjectID, permissions: permissions}
}

func validSubject(subject string) bool {
	if len(subject) < 1 || len(subject) > 128 || strings.TrimSpace(subject) != subject {
		return false
	}
	for _, character := range subject {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func validSecret(secret string, minimum, maximum int) bool {
	if len(secret) < minimum || len(secret) > maximum || strings.TrimSpace(secret) != secret {
		return false
	}
	for _, character := range secret {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
