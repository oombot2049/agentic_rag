package idempotency

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticrag/agenticrag/internal/domain"
)

var testNow = time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)

func TestClassifyAllRecordStates(t *testing.T) {
	t.Parallel()
	hash := mustFingerprint(t, "CREATE_TURN")
	other := mustFingerprint(t, "CREATE_RUN")
	tests := []struct {
		status   Status
		incoming Fingerprint
		want     Decision
	}{
		{StatusInProgress, hash, DecisionInProgress},
		{StatusSucceeded, hash, DecisionReplaySucceeded},
		{StatusFailedFinal, hash, DecisionReplayFailedFinal},
		{Status("FUTURE"), hash, DecisionKeyReused},
		{StatusSucceeded, other, DecisionKeyReused},
	}
	for _, test := range tests {
		if got := classify(Record{Status: test.status, Fingerprint: hash}, test.incoming); got != test.want {
			t.Errorf("classify(%s) = %s, want %s", test.status, got, test.want)
		}
	}
}

func TestClaimValidation(t *testing.T) {
	t.Parallel()
	valid := validScope()
	hash := mustFingerprint(t, valid.Operation)
	tests := []struct {
		name        string
		scope       Scope
		fingerprint Fingerprint
		now         time.Time
		retention   time.Duration
	}{
		{"tenant", Scope{TenantID: "bad", AuthenticatedSubjectID: valid.AuthenticatedSubjectID, Operation: valid.Operation, Key: valid.Key}, hash, testNow, MinimumRetention},
		{"subject", Scope{TenantID: valid.TenantID, Operation: valid.Operation, Key: valid.Key}, hash, testNow, MinimumRetention},
		{"subject control", Scope{TenantID: valid.TenantID, AuthenticatedSubjectID: "user\nadmin", Operation: valid.Operation, Key: valid.Key}, hash, testNow, MinimumRetention},
		{"operation", Scope{TenantID: valid.TenantID, AuthenticatedSubjectID: valid.AuthenticatedSubjectID, Operation: "create turn", Key: valid.Key}, hash, testNow, MinimumRetention},
		{"short key", Scope{TenantID: valid.TenantID, AuthenticatedSubjectID: valid.AuthenticatedSubjectID, Operation: valid.Operation, Key: "short"}, hash, testNow, MinimumRetention},
		{"key control", Scope{TenantID: valid.TenantID, AuthenticatedSubjectID: valid.AuthenticatedSubjectID, Operation: valid.Operation, Key: "0123456789abcde\n"}, hash, testNow, MinimumRetention},
		{"zero hash", valid, Fingerprint{}, testNow, MinimumRetention},
		{"zero time", valid, hash, time.Time{}, MinimumRetention},
		{"short retention", valid, hash, testNow, MinimumRetention - time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateClaim(test.scope, test.fingerprint, test.now, test.retention); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if err := validateClaim(valid, hash, testNow, MinimumRetention); err != nil {
		t.Fatalf("valid claim: %v", err)
	}
}

func TestResponseValidation(t *testing.T) {
	t.Parallel()
	validSuccess := Response{StatusCode: 202, Body: json.RawMessage(`{"run_id":"run_0001"}`), Location: "/v1/runs/run_0001", ResourceType: "RUN", ResourceID: "run_0001"}
	if err := validateResponse(StatusSucceeded, validSuccess); err != nil {
		t.Fatal(err)
	}
	if err := validateResponse(StatusFailedFinal, Response{StatusCode: 409, Body: json.RawMessage(`{"error":{"code":"SESSION_BUSY"}}`)}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		status   Status
		response Response
	}{
		{"bad JSON", StatusSucceeded, Response{StatusCode: 200, Body: json.RawMessage(`{`)}},
		{"oversized", StatusSucceeded, Response{StatusCode: 200, Body: json.RawMessage(`"` + strings.Repeat("x", maxResponseBytes) + `"`)}},
		{"success status", StatusSucceeded, Response{StatusCode: 409, Body: json.RawMessage(`{}`)}},
		{"failure status", StatusFailedFinal, Response{StatusCode: 202, Body: json.RawMessage(`{}`)}},
		{"resource pair", StatusSucceeded, Response{StatusCode: 200, Body: json.RawMessage(`{}`), ResourceType: "RUN"}},
		{"location control", StatusSucceeded, Response{StatusCode: 200, Body: json.RawMessage(`{}`), Location: "bad\nlocation"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateResponse(test.status, test.response); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRepositoryRejectsNilExecutor(t *testing.T) {
	t.Parallel()
	if _, err := NewRepository(nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("error = %v", err)
	}
}

func TestNullable(t *testing.T) {
	t.Parallel()
	if nullable("") != nil || nullable("value") != "value" {
		t.Fatal("nullable conversion mismatch")
	}
}

func validScope() Scope {
	return Scope{TenantID: domain.TenantID("ten_0001"), AuthenticatedSubjectID: "user-42", Operation: "CREATE_TURN", Key: "idem_0123456789abcdef"}
}

func mustFingerprint(t *testing.T, operation string) Fingerprint {
	t.Helper()
	fingerprint, err := NewFingerprint(Request{Operation: operation, Body: struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}
