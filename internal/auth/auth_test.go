package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/apperror"
	"github.com/agenticrag/agenticrag/internal/domain"
)

func principal(t *testing.T, tenant, subject string, permissions ...string) Principal {
	t.Helper()
	value, err := NewPrincipal(domain.TenantID(tenant), subject, permissions)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPrincipalValidationCopiesAndContext(t *testing.T) {
	t.Parallel()
	permissions := []string{"sessions:write", "sessions:read", "sessions:read"}
	value, err := NewPrincipal("ten_0001", "sub_0001", permissions)
	if err != nil {
		t.Fatal(err)
	}
	permissions[0] = "admin"
	got := value.Permissions()
	if strings.Join(got, ",") != "sessions:read,sessions:write" || value.HasPermission("admin") {
		t.Fatalf("permissions = %v", got)
	}
	got[0] = "admin"
	if value.HasPermission("admin") {
		t.Fatal("permission getter leaked mutable state")
	}
	ctx := withPrincipal(context.Background(), value)
	loaded, ok := PrincipalFromContext(ctx)
	if !ok || loaded.TenantID() != "ten_0001" || loaded.SubjectID() != "sub_0001" {
		t.Fatalf("context principal = %+v/%v", loaded, ok)
	}
	if tenant, ok := TenantID(ctx); !ok || tenant != "ten_0001" {
		t.Fatalf("TenantID = %s/%v", tenant, ok)
	}
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Fatal("empty context produced principal")
	}
	for _, test := range []struct {
		tenant, subject string
		permissions     []string
	}{{"bad", "sub_1", nil}, {"ten_0001", "", nil}, {"ten_0001", "subject admin", nil}, {"ten_0001", "sub_1", []string{"Admin"}}} {
		if _, err := NewPrincipal(domain.TenantID(test.tenant), test.subject, test.permissions); !errors.Is(err, ErrInvalidPrincipal) {
			t.Errorf("NewPrincipal(%q,%q) = %v", test.tenant, test.subject, err)
		}
	}
}

func TestExtractCredentialStrictMatrix(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		headers http.Header
		kind    CredentialKind
		value   string
		wantErr error
	}{
		{"bearer", http.Header{"Authorization": {"Bearer token-value"}}, CredentialBearer, "token-value", nil},
		{"bearer case", http.Header{"Authorization": {"bearer token-value"}}, CredentialBearer, "token-value", nil},
		{"api key", http.Header{"X-Api-Key": {"0123456789abcdef"}}, CredentialAPIKey, "0123456789abcdef", nil},
		{"missing", http.Header{}, "", "", ErrMissingCredential},
		{"scheme", http.Header{"Authorization": {"Basic abc"}}, "", "", ErrMalformedCredential},
		{"empty", http.Header{"Authorization": {"Bearer"}}, "", "", ErrMalformedCredential},
		{"comma", http.Header{"Authorization": {"Bearer a,Bearer b"}}, "", "", ErrMalformedCredential},
		{"duplicate", http.Header{"Authorization": {"Bearer a", "Bearer b"}}, "", "", ErrMalformedCredential},
		{"ambiguous", http.Header{"Authorization": {"Bearer a"}, "X-Api-Key": {"0123456789abcdef"}}, "", "", ErrMalformedCredential},
		{"short api", http.Header{"X-Api-Key": {"short"}}, "", "", ErrMalformedCredential},
		{"api whitespace", http.Header{"X-Api-Key": {"01234567 9abcdef"}}, "", "", ErrMalformedCredential},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header = test.headers
			credential, err := ExtractCredential(request)
			if !errors.Is(err, test.wantErr) || credential.Kind != test.kind || credential.Value != test.value {
				t.Fatalf("Extract = %+v/%v", credential, err)
			}
		})
	}
}

func TestMiddlewareTrustedTenantAndPermission(t *testing.T) {
	t.Parallel()
	trusted := principal(t, "ten_0001", "sub_0001", "sessions:read")
	authenticator, _ := NewAuthenticator(VerifierFunc(func(_ context.Context, token string) (Principal, error) {
		if token != "valid-token" {
			return Principal{}, ErrInvalidCredential
		}
		return trusted, nil
	}), nil)
	authLayer, _ := Middleware(authenticator, nil)
	permissionLayer, _ := RequirePermission("sessions:read", nil)
	called := false
	handler := authLayer(permissionLayer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		called = true
		tenant, ok := TenantID(request.Context())
		if !ok || tenant != "ten_0001" {
			t.Fatalf("trusted tenant = %s/%v", tenant, ok)
		}
		if request.Header.Get("X-Tenant-ID") != "" || request.Header.Get("X-User-ID") != "" || request.Header.Get("Authorization") != "" {
			t.Fatal("credential or spoofed identity headers reached handler")
		}
		response.WriteHeader(http.StatusNoContent)
	})))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	request.Header.Set("X-Tenant-ID", "ten_9999")
	request.Header.Set("X-User-ID", "admin")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || !called {
		t.Fatalf("status=%d called=%v", recorder.Code, called)
	}
	if request.Header.Get("X-Tenant-ID") != "ten_9999" {
		t.Fatal("middleware mutated caller-owned request")
	}
}

func TestMiddlewareFailuresAreSafeAndStable(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		verify error
		header string
		status int
		code   string
	}{
		{"missing", nil, "", http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"invalid", ErrInvalidCredential, "Bearer secret-invalid-token", http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"unavailable", ErrVerifierUnavailable, "Bearer secret-outage-token", http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
		{"internal", errors.New("secret provider detail"), "Bearer secret-bug-token", http.StatusInternalServerError, "INTERNAL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authenticator, _ := NewAuthenticator(VerifierFunc(func(context.Context, string) (Principal, error) {
				return Principal{}, test.verify
			}), nil)
			layer, _ := Middleware(authenticator, nil)
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			recorder := httptest.NewRecorder()
			layer(http.NotFoundHandler()).ServeHTTP(recorder, request)
			if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("response=%d %s", recorder.Code, recorder.Body.String())
			}
			for _, secret := range []string{"secret-invalid-token", "secret-outage-token", "secret-bug-token", "secret provider detail"} {
				if strings.Contains(recorder.Body.String(), secret) {
					t.Fatalf("response leaked %q", secret)
				}
			}
			if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("security response headers missing")
			}
		})
	}
}

type recordingAPIKeyStore struct {
	mu     sync.Mutex
	digest [sha256.Size]byte
	record APIKeyRecord
	err    error
}

func (s *recordingAPIKeyStore) ResolveAPIKey(_ context.Context, digest [sha256.Size]byte) (APIKeyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.digest = digest
	return s.record, s.err
}

func TestAPIKeyVerifierHashesExpiresRevokesAndFailsClosed(t *testing.T) {
	t.Parallel()
	var nilStore *recordingAPIKeyStore
	if _, err := NewAPIKeyVerifier(nilStore); err == nil {
		t.Fatal("typed-nil API key store accepted")
	}
	raw := "api-key-secret-0123456789"
	validPrincipal := principal(t, "ten_0001", "sub_api", "sessions:read")
	store := &recordingAPIKeyStore{record: APIKeyRecord{Principal: validPrincipal, ExpiresAt: time.Now().Add(time.Hour)}}
	verifier, err := NewAPIKeyVerifier(store)
	if err != nil {
		t.Fatal(err)
	}
	verifier.now = func() time.Time { return time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC) }
	store.record.ExpiresAt = verifier.now().Add(time.Hour)
	got, err := verifier.Verify(context.Background(), raw)
	if err != nil || got.TenantID() != "ten_0001" {
		t.Fatalf("Verify = %+v/%v", got, err)
	}
	if store.digest != sha256.Sum256([]byte(raw)) || strings.Contains(fmt.Sprintf("%x", store.digest), raw) {
		t.Fatal("API key was not reduced to its digest")
	}
	for _, test := range []struct {
		name   string
		record APIKeyRecord
		err    error
		want   error
	}{
		{"unknown", APIKeyRecord{}, ErrCredentialNotFound, ErrInvalidCredential},
		{"revoked", APIKeyRecord{Principal: validPrincipal, ExpiresAt: verifier.now().Add(time.Hour), Revoked: true}, nil, ErrInvalidCredential},
		{"expired", APIKeyRecord{Principal: validPrincipal, ExpiresAt: verifier.now()}, nil, ErrInvalidCredential},
		{"store outage", APIKeyRecord{}, errors.New("database down"), ErrVerifierUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			store.record, store.err = test.record, test.err
			_, err := verifier.Verify(context.Background(), raw)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := verifier.Verify(context.Background(), "short"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("short key error = %v", err)
	}
}

func TestForeignAndAbsentResourcesAreByteIdentical(t *testing.T) {
	t.Parallel()
	trusted := principal(t, "ten_0001", "sub_0001", "sessions:read")
	authenticator, _ := NewAuthenticator(VerifierFunc(func(context.Context, string) (Principal, error) { return trusted, nil }), nil)
	layer, _ := Middleware(authenticator, nil)
	owners := map[string]string{"ses_foreign": "ten_0002"}
	handler := layer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		tenant, _ := TenantID(request.Context())
		id := strings.TrimPrefix(request.URL.Path, "/sessions/")
		if owner, exists := owners[id]; !exists || owner != tenant.String() {
			WriteFailure(response, request, apperror.New(apperror.CodeSessionNotFound, false, nil))
			return
		}
	}))
	responses := make([]*httptest.ResponseRecorder, 0, 2)
	for _, id := range []string{"ses_absent", "ses_foreign"} {
		request := httptest.NewRequest(http.MethodGet, "/sessions/"+id, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		responses = append(responses, recorder)
	}
	if responses[0].Code != http.StatusNotFound || responses[1].Code != responses[0].Code || responses[1].Body.String() != responses[0].Body.String() {
		t.Fatalf("enumerable responses: absent=%d/%q foreign=%d/%q", responses[0].Code, responses[0].Body.String(), responses[1].Code, responses[1].Body.String())
	}
}

func TestPermissionDenialMissingContextAndAuthenticatorValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewAuthenticator(nil, nil); err == nil {
		t.Fatal("empty authenticator accepted")
	}
	var nilVerifier VerifierFunc
	if _, err := NewAuthenticator(nilVerifier, nil); err == nil {
		t.Fatal("typed-nil authenticator accepted")
	}
	trusted := principal(t, "ten_0001", "sub_0001", "sessions:read")
	authenticator, _ := NewAuthenticator(VerifierFunc(func(context.Context, string) (Principal, error) { return trusted, nil }), nil)
	if _, err := authenticator.Authenticate(context.Background(), Credential{Kind: CredentialAPIKey, Value: "0123456789abcdef"}); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("missing adapter error = %v", err)
	}
	invalidAuthenticator, _ := NewAuthenticator(VerifierFunc(func(context.Context, string) (Principal, error) { return Principal{}, nil }), nil)
	if _, err := invalidAuthenticator.Authenticate(context.Background(), Credential{Kind: CredentialBearer, Value: "token"}); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("invalid verifier principal = %v", err)
	}
	if _, err := RequirePermission("ADMIN", nil); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("invalid permission error = %v", err)
	}
	layer, _ := RequirePermission("sessions:write", nil)
	for _, test := range []struct {
		name   string
		ctx    context.Context
		status int
	}{
		{"missing", context.Background(), http.StatusUnauthorized},
		{"denied", withPrincipal(context.Background(), trusted), http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(test.ctx)
			recorder := httptest.NewRecorder()
			layer(http.NotFoundHandler()).ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status = %d", recorder.Code)
			}
		})
	}
}

func TestConcurrentPrincipalIsolation(t *testing.T) {
	t.Parallel()
	principals := map[string]Principal{
		"token-a": principal(t, "ten_0001", "sub_a", "sessions:read"),
		"token-b": principal(t, "ten_0002", "sub_b", "sessions:read"),
	}
	authenticator, _ := NewAuthenticator(VerifierFunc(func(_ context.Context, token string) (Principal, error) {
		value, ok := principals[token]
		if !ok {
			return Principal{}, ErrInvalidCredential
		}
		return value, nil
	}), nil)
	layer, _ := Middleware(authenticator, nil)
	handler := layer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		tenant, _ := TenantID(request.Context())
		if request.URL.Path != "/"+tenant.String() {
			response.WriteHeader(http.StatusConflict)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	var wait sync.WaitGroup
	failures := make(chan string, 100)
	for index := 0; index < 100; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			token, tenant := "token-a", "ten_0001"
			if index%2 == 1 {
				token, tenant = "token-b", "ten_0002"
			}
			request := httptest.NewRequest(http.MethodGet, "/"+tenant, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNoContent {
				failures <- fmt.Sprintf("request %d status %d", index, recorder.Code)
			}
		}(index)
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}
