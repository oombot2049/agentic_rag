package redaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMandatoryCredentialAndPIIDetection(t *testing.T) {
	service := mustStaticService(t, StrictPolicy())
	values := []string{
		"Authorization: Bearer abcdefghijklmnop",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature1234",
		"key sk-abcdefghijklmnopqrstuvwxyz",
		"aws AKIAABCDEFGHIJKLMNOP",
		"password=correct-horse-battery-staple",
		"person@example.com",
		"+86 138 0013 8000",
	}
	for _, value := range values {
		result := service.Redact(context.Background(), "ten_0001", BoundaryLog, value)
		if strings.Contains(result.Value, value) || !strings.Contains(result.Value, Replacement) || len(result.Findings) == 0 {
			t.Errorf("Redact(%q) = %+v", value, result)
		}
	}
	safe := "prompt_hash=abc123 token_count=42 artifact_id=art_0001"
	if result := service.Redact(context.Background(), "ten_0001", BoundaryLog, safe); result.Value != safe {
		t.Fatalf("safe metadata changed to %q", result.Value)
	}
}

func TestSensitiveFieldsAndNestedJSON(t *testing.T) {
	service := mustStaticService(t, StrictPolicy())
	field := service.RedactField(context.Background(), "ten_0001", BoundaryLog, "provider.api-key", "not-even-token-shaped")
	if field.Value != Replacement || field.Findings[0].Kind != FindingSensitiveKey {
		t.Fatalf("RedactField() = %+v", field)
	}
	input := `{"safe":"hello","nested":{"password":{"deep":"private"},"owner":"person@example.com"},"count":4}`
	result := service.RedactJSON(context.Background(), "ten_0001", BoundaryArtifactPreview, []byte(input))
	for _, forbidden := range []string{"private", "person@example.com"} {
		if strings.Contains(result.Value, forbidden) {
			t.Fatalf("RedactJSON() leaked %q: %s", forbidden, result.Value)
		}
	}
	for _, required := range []string{`"safe":"hello"`, `"count":4`, Replacement} {
		if !strings.Contains(result.Value, required) {
			t.Fatalf("RedactJSON() omitted %q: %s", required, result.Value)
		}
	}
	malformed := service.RedactJSON(context.Background(), "ten_0001", BoundaryErrorDetail, []byte(`{"x":"sk-abcdefghijklmnop"`))
	if strings.Contains(malformed.Value, "sk-abcdefghijklmnop") {
		t.Fatalf("malformed JSON leaked credential: %s", malformed.Value)
	}
}

func TestTenantRulesAreIsolatedAndResolverFailureIsStrict(t *testing.T) {
	custom, err := NewPolicy(Config{
		Version: "tenant-a-v1", CustomRules: []Rule{{Name: "account", Pattern: `ACME-[0-9]{6}`}},
		SensitiveArtifactMaxTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := NewPolicy(Config{Version: "tenant-b-v1", SensitiveArtifactMaxTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ResolverFunc(func(_ context.Context, tenant string) (*Policy, error) {
		switch tenant {
		case "ten_a":
			return custom, nil
		case "ten_b":
			return baseline, nil
		default:
			return nil, ErrPolicyUnavailable
		}
	}), StrictPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got := service.Redact(context.Background(), "ten_a", BoundaryLog, "ACME-123456"); got.Value != Replacement || got.PolicyVersion != "tenant-a-v1" {
		t.Fatalf("tenant A result = %+v", got)
	}
	if got := service.Redact(context.Background(), "ten_b", BoundaryLog, "ACME-123456"); got.Value != "ACME-123456" {
		t.Fatalf("tenant B result = %+v", got)
	}
	failed := service.Redact(context.Background(), "ten_missing", BoundaryLog, "person@example.com")
	if !failed.Degraded || failed.Value != Replacement || failed.PolicyVersion != "strict-v1" {
		t.Fatalf("fallback result = %+v", failed)
	}

	const workers = 200
	var group sync.WaitGroup
	group.Add(workers)
	for index := range workers {
		go func() {
			defer group.Done()
			tenant := "ten_a"
			want := Replacement
			if index%2 == 1 {
				tenant = "ten_b"
				want = "ACME-123456"
			}
			if got := service.Redact(context.Background(), tenant, BoundaryLog, "ACME-123456").Value; got != want {
				t.Errorf("tenant %s got %q, want %q", tenant, got, want)
			}
		}()
	}
	group.Wait()
}

func TestArtifactPolicyRequiresStrongEncryptionAndShortTTL(t *testing.T) {
	policy, err := NewPolicy(Config{Version: "artifact-v1", SensitiveArtifactMaxTTL: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	service := mustStaticService(t, policy)
	request := ArtifactRequest{TenantID: "ten_0001", Sensitivity: ArtifactRestricted, EncryptionMode: "SSE-KMS", TTL: time.Hour}
	decision, err := service.AuthorizeArtifact(context.Background(), request)
	if err != nil || !decision.Allowed || decision.MaximumTTL != 2*time.Hour {
		t.Fatalf("AuthorizeArtifact() = %+v, %v", decision, err)
	}
	request.EncryptionMode = "SSE-S3"
	if _, err := service.AuthorizeArtifact(context.Background(), request); !errors.Is(err, ErrArtifactPolicy) {
		t.Fatalf("weak encryption error = %v", err)
	}
	request.EncryptionMode = "CLIENT"
	request.TTL = 3 * time.Hour
	if denied, err := service.AuthorizeArtifact(context.Background(), request); !errors.Is(err, ErrArtifactPolicy) || denied.MaximumTTL != 2*time.Hour {
		t.Fatalf("long TTL result = %+v, %v", denied, err)
	}
	request.Sensitivity = ArtifactPublic
	if decision, err := service.AuthorizeArtifact(context.Background(), request); err != nil || !decision.Allowed {
		t.Fatalf("public artifact result = %+v, %v", decision, err)
	}
}

func TestInvalidPoliciesAndFailClosedLimits(t *testing.T) {
	tests := []Config{
		{},
		{Version: "v1", SensitiveArtifactMaxTTL: time.Second},
		{Version: "v1", SensitiveArtifactMaxTTL: time.Hour, CustomRules: []Rule{{Name: "bad", Pattern: "("}}},
		{Version: "v1", SensitiveArtifactMaxTTL: time.Hour, CustomRules: []Rule{{Name: "empty", Pattern: "a*"}}},
		{Version: "v1", SensitiveArtifactMaxTTL: time.Hour, CustomRules: []Rule{{Name: "marker", Pattern: "REDACTED"}}},
		{Version: "v1", SensitiveArtifactMaxTTL: time.Hour, SensitiveKeys: []string{" "}},
	}
	for index, config := range tests {
		if _, err := NewPolicy(config); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("NewPolicy(%d) error = %v", index, err)
		}
	}
	if _, err := NewService(nil, StrictPolicy()); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("NewService(nil) error = %v", err)
	}
	if _, err := NewStatic(nil); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("NewStatic(nil) error = %v", err)
	}
	service := mustStaticService(t, StrictPolicy())
	oversized := strings.Repeat("x", (64<<10)+1)
	if result := service.Redact(context.Background(), "ten_0001", BoundaryLog, oversized); result.Value != Replacement || !result.Degraded {
		t.Fatalf("oversized result = %+v", result)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if result := service.Redact(cancelled, "ten_0001", BoundaryLog, "safe"); result.Value != Replacement || !result.Degraded {
		t.Fatalf("cancelled result = %+v", result)
	}
	if result := service.Redact(context.Background(), "ten_0001", Boundary("UNKNOWN"), "safe"); result.Value != Replacement {
		t.Fatalf("unknown boundary result = %+v", result)
	}
}

func TestFieldAndArtifactFailureEdges(t *testing.T) {
	policy, err := NewPolicy(Config{
		Version: "field-v1", SensitiveKeys: []string{"customer_reference"},
		SensitiveArtifactMaxTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := mustStaticService(t, policy)
	if !service.SensitiveKey(context.Background(), "ten_0001", "customer-reference") ||
		service.SensitiveKey(context.Background(), "ten_0001", "artifact_id") {
		t.Fatal("SensitiveKey() classification mismatch")
	}
	result := service.RedactField(context.Background(), "ten_0001", BoundaryLog, "note", "sk-abcdefghijklmnop")
	if result.Value != Replacement || len(result.Findings) == 0 {
		t.Fatalf("content RedactField() = %+v", result)
	}
	if result := service.RedactField(context.Background(), "ten_0001", Boundary("bad"), "note", "safe"); result.Value != Replacement || !result.Degraded {
		t.Fatalf("invalid-boundary RedactField() = %+v", result)
	}
	if result := service.RedactJSON(context.Background(), "ten_0001", BoundaryLog, []byte(strings.Repeat("x", (64<<10)+1))); result.Value != Replacement {
		t.Fatalf("oversized RedactJSON() = %+v", result)
	}
	if _, err := service.AuthorizeArtifact(context.Background(), ArtifactRequest{
		TenantID: "ten_0001", Sensitivity: ArtifactSensitivity("UNKNOWN"), EncryptionMode: "SSE-KMS", TTL: time.Minute,
	}); !errors.Is(err, ErrArtifactPolicy) {
		t.Fatalf("unknown Artifact sensitivity error = %v", err)
	}
	if _, err := service.AuthorizeArtifact(context.Background(), ArtifactRequest{
		TenantID: "ten_0001", Sensitivity: ArtifactInternal, TTL: 0,
	}); !errors.Is(err, ErrArtifactPolicy) {
		t.Fatalf("zero Artifact TTL error = %v", err)
	}
	outage, err := NewService(ResolverFunc(func(context.Context, string) (*Policy, error) {
		return nil, ErrPolicyUnavailable
	}), StrictPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outage.AuthorizeArtifact(context.Background(), ArtifactRequest{
		TenantID: "ten_0001", Sensitivity: ArtifactRestricted, EncryptionMode: "SSE-KMS", TTL: time.Minute,
	}); !errors.Is(err, ErrArtifactPolicy) {
		t.Fatalf("outage Artifact error = %v", err)
	}
	invalidResolver, err := NewService(ResolverFunc(func(context.Context, string) (*Policy, error) {
		return &Policy{}, nil
	}), StrictPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if result := invalidResolver.Redact(context.Background(), "ten_0001", BoundaryLog, "person@example.com"); !result.Degraded || result.Value != Replacement {
		t.Fatalf("invalid resolved policy result = %+v", result)
	}
}

func TestJSONShapeLimitFailsClosed(t *testing.T) {
	service := mustStaticService(t, StrictPolicy())
	value := "0"
	for range maximumDepth + 2 {
		value = "[" + value + "]"
	}
	result := service.RedactJSON(context.Background(), "ten_0001", BoundaryArtifactPreview, []byte(value))
	if result.Value != Replacement || !result.Degraded {
		t.Fatalf("deep JSON result = %+v", result)
	}
}

func mustStaticService(t *testing.T, policy *Policy) *Service {
	t.Helper()
	service, err := NewStatic(policy)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func ExampleService_Redact() {
	service, _ := NewStatic(StrictPolicy())
	result := service.Redact(context.Background(), "ten_0001", BoundaryLog, "token=private-value")
	fmt.Println(result.Value)
	// Output: [REDACTED]
}
