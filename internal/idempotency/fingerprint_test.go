package idempotency

import (
	"encoding/json"
	"errors"
	"testing"
)

type normalizedTurn struct {
	ExpectedContextVersion uint64              `json:"expected_context_version"`
	Message                map[string]string   `json:"message"`
	Execution              normalizedExecution `json:"execution"`
}

type normalizedVersion struct {
	Version uint64 `json:"version"`
}

func TestFingerprintPreservesLargeJSONNumbers(t *testing.T) {
	t.Parallel()
	fromRaw, err := NewFingerprint(Request{Operation: "CREATE_TURN", Body: json.RawMessage(`{"version":9007199254740993}`)})
	if err != nil {
		t.Fatal(err)
	}
	fromTyped, err := NewFingerprint(Request{Operation: "CREATE_TURN", Body: normalizedVersion{Version: 9007199254740993}})
	if err != nil {
		t.Fatal(err)
	}
	if fromRaw != fromTyped {
		t.Fatalf("large integer lost precision: %s != %s", fromRaw, fromTyped)
	}
}

type normalizedExecution struct {
	MaxSteps int `json:"max_steps"`
}

func TestFingerprintCanonicalizesBodyAndPathOrdering(t *testing.T) {
	t.Parallel()
	first, err := NewFingerprint(Request{
		Operation: "CREATE_TURN", PathParameters: map[string]string{"session_id": "ses_0001", "z": "last"},
		Body: map[string]any{"message": map[string]any{"content": "hello"}, "expected_context_version": 7, "execution": map[string]any{"max_steps": 12}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFingerprint(Request{
		Operation: "CREATE_TURN", PathParameters: map[string]string{"z": "last", "session_id": "ses_0001"},
		Body: normalizedTurn{ExpectedContextVersion: 7, Message: map[string]string{"content": "hello"}, Execution: normalizedExecution{MaxSteps: 12}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("canonical hashes differ: %s != %s", first, second)
	}
	if len(first.String()) != 64 {
		t.Fatalf("hex fingerprint length = %d", len(first.String()))
	}
	bytesCopy := first.Bytes()
	bytesCopy[0] ^= 0xff
	if first.Bytes()[0] == bytesCopy[0] {
		t.Fatal("Fingerprint.Bytes returned mutable backing storage")
	}
}

func TestFingerprintChangesForBusinessSemantics(t *testing.T) {
	t.Parallel()
	base := Request{Operation: "CREATE_TURN", PathParameters: map[string]string{"session_id": "ses_0001"}, Body: map[string]any{"max_steps": 12}}
	want, err := NewFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(Request) Request
	}{
		{"operation", func(r Request) Request { r.Operation = "CREATE_RUN"; return r }},
		{"path value", func(r Request) Request { r.PathParameters = map[string]string{"session_id": "ses_0002"}; return r }},
		{"path name", func(r Request) Request { r.PathParameters = map[string]string{"turn_id": "ses_0001"}; return r }},
		{"body", func(r Request) Request { r.Body = map[string]any{"max_steps": 13}; return r }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewFingerprint(test.mutate(base))
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatal("business-semantic change did not change fingerprint")
			}
		})
	}
}

func TestFingerprintRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	tests := []Request{
		{Operation: "create turn", Body: struct{}{}},
		{Operation: "CREATE_TURN", PathParameters: map[string]string{"": "value"}, Body: struct{}{}},
		{Operation: "CREATE_TURN", Body: func() {}},
	}
	for _, request := range tests {
		if _, err := NewFingerprint(request); err == nil {
			t.Errorf("NewFingerprint(%+v) error = nil", request)
		}
	}
}

func TestFingerprintFromBytes(t *testing.T) {
	t.Parallel()
	fingerprint, err := NewFingerprint(Request{Operation: "CREATE_SESSION", Body: struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fingerprintFromBytes(fingerprint.Bytes())
	if err != nil || got != fingerprint {
		t.Fatalf("round trip = %v/%v", got, err)
	}
	if _, err := fingerprintFromBytes([]byte("short")); err == nil {
		t.Fatal("short hash error = nil")
	}
	if !errors.Is(validateOperation("bad"), ErrInvalidInput) {
		t.Fatal("operation error does not wrap ErrInvalidInput")
	}
}
