package apperror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestRegistryContract(t *testing.T) {
	wantStatus := map[Code]int{
		CodeInvalidArgument: http.StatusBadRequest, CodeInvalidIdempotencyKey: http.StatusBadRequest,
		CodeInvalidEventCursor: http.StatusBadRequest, CodeUnauthenticated: http.StatusUnauthorized,
		CodePermissionDenied: http.StatusForbidden, CodeSessionNotFound: http.StatusNotFound,
		CodeTurnNotFound: http.StatusNotFound, CodeRunNotFound: http.StatusNotFound,
		CodeSessionVersionConflict: http.StatusConflict, CodeSessionBusy: http.StatusConflict,
		CodeIdempotencyInProgress: http.StatusConflict, CodeIdempotencyKeyReused: http.StatusConflict,
		CodeRunFenced: http.StatusConflict, CodeEventCursorExpired: http.StatusGone,
		CodeResourceDeleted: http.StatusGone, CodeTenantQuotaExceeded: http.StatusTooManyRequests,
		CodeGlobalCapacityExceeded: http.StatusTooManyRequests, CodeInternal: http.StatusInternalServerError,
		CodeProviderUnavailable: http.StatusBadGateway, CodeRetrievalUnavailable: http.StatusServiceUnavailable,
		CodeServiceUnavailable: http.StatusServiceUnavailable, CodeRunDeadlineExceeded: http.StatusGatewayTimeout,
		CodeProviderTimeout: http.StatusGatewayTimeout,
	}
	codePattern := regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)
	seen := make(map[Code]bool, len(wantStatus))
	for _, code := range Codes() {
		t.Run(string(code), func(t *testing.T) {
			if seen[code] {
				t.Fatalf("duplicate code %s", code)
			}
			seen[code] = true
			if !code.Valid() || !codePattern.MatchString(string(code)) {
				t.Fatalf("invalid code %q", code)
			}
			err := New(code, true, nil)
			if err.HTTPStatus() != wantStatus[code] {
				t.Errorf("status = %d, want %d", err.HTTPStatus(), wantStatus[code])
			}
			public := Public(err)
			if public.Code != code || public.Message == "" || !public.Retryable {
				t.Errorf("public = %+v", public)
			}
			if !strings.HasPrefix(err.Error(), string(code)+": ") {
				t.Errorf("Error() = %q", err.Error())
			}
		})
	}
	if len(seen) != len(wantStatus) {
		t.Fatalf("registry has %d codes, mapping test has %d", len(seen), len(wantStatus))
	}
}

func TestRetryabilityIsOccurrenceSpecific(t *testing.T) {
	retryable := New(CodeSessionBusy, true, nil)
	notRetryable := New(CodeSessionBusy, false, nil)
	if !retryable.Retryable() || notRetryable.Retryable() {
		t.Fatalf("retry flags: true=%t false=%t", retryable.Retryable(), notRetryable.Retryable())
	}
	if retryable.HTTPStatus() != notRetryable.HTTPStatus() {
		t.Fatal("HTTP mapping changed with retryability")
	}
}

func TestErrorsIsAndAsPreserveIdentityAndCause(t *testing.T) {
	root := errors.New("database marker: private")
	applicationError := New(CodeSessionBusy, true, root)
	wrapped := fmt.Errorf("application layer: %w", applicationError)
	if !errors.Is(wrapped, root) {
		t.Fatal("errors.Is did not reach root cause")
	}
	if !errors.Is(wrapped, New(CodeSessionBusy, false, nil)) || !IsCode(wrapped, CodeSessionBusy) {
		t.Fatal("stable code identity did not match")
	}
	if errors.Is(wrapped, New(CodeRunFenced, true, nil)) || IsCode(wrapped, Code("UNKNOWN")) {
		t.Fatal("unrelated/invalid code matched")
	}
	var got *Error
	if !errors.As(wrapped, &got) || got != applicationError {
		t.Fatalf("errors.As = %p, want %p", got, applicationError)
	}
	if Normalize(wrapped) != applicationError {
		t.Fatal("Normalize did not preserve application error")
	}
}

func TestInternalCauseNeverEntersPublicRepresentations(t *testing.T) {
	marker := "password=super-secret SQL SELECT private_table"
	err := New(CodeRetrievalUnavailable, true, errors.New(marker))
	public := Public(err)
	encoded, jsonErr := json.Marshal(public)
	if jsonErr != nil {
		t.Fatal(jsonErr)
	}
	for name, value := range map[string]string{"Error": err.Error(), "PublicJSON": string(encoded), "PublicMessage": public.Message} {
		if strings.Contains(value, marker) || strings.Contains(value, "super-secret") || strings.Contains(value, "private_table") {
			t.Fatalf("%s leaked cause: %q", name, value)
		}
	}
	if !errors.Is(err, err.Unwrap()) {
		t.Fatal("private cause is not available through errors.Is")
	}
}

func TestUnknownAndNilNormalization(t *testing.T) {
	unknown := errors.New("provider returned a private payload")
	normalized := Normalize(unknown)
	if normalized.Code() != CodeInternal || normalized.Retryable() || !errors.Is(normalized, unknown) {
		t.Fatalf("Normalize() = %#v", normalized)
	}
	invalid := New(Code("NOT_REGISTERED"), true, nil)
	if invalid.Code() != CodeInternal || invalid.Retryable() || invalid.HTTPStatus() != http.StatusInternalServerError {
		t.Fatalf("invalid code fallback = %#v", invalid)
	}
	if Normalize(nil) != nil || HTTPStatus(nil) != http.StatusInternalServerError {
		t.Fatal("nil behavior is not safe")
	}
	if public := Public(nil); public.Code != CodeInternal || public.Retryable {
		t.Fatalf("Public(nil) = %+v", public)
	}
}

func TestCodesReturnsCopy(t *testing.T) {
	codes := Codes()
	codes[0] = CodeInternal
	if Codes()[0] != CodeInvalidArgument {
		t.Fatal("Codes exposed mutable registry order")
	}
}

func TestDefensiveReceiverFallbacks(t *testing.T) {
	var nilError *Error
	if nilError.Code() != CodeInternal || nilError.Retryable() || nilError.Unwrap() != nil {
		t.Fatal("nil receiver did not fall back safely")
	}
	if nilError.HTTPStatus() != http.StatusInternalServerError || nilError.PublicMessage() == "" {
		t.Fatal("nil receiver public mapping is invalid")
	}
	if nilError.Is(nil) {
		t.Fatal("nil target unexpectedly matched")
	}

	malformed := &Error{code: Code("MALFORMED"), retryable: true}
	if malformed.Code() != CodeInternal || malformed.HTTPStatus() != http.StatusInternalServerError {
		t.Fatalf("malformed receiver = %#v", malformed)
	}
	if got := specificationFor(Code("MISSING")); got != specifications[CodeInternal] {
		t.Fatalf("invalid specification fallback = %+v", got)
	}

	cause := errors.New("private invalid-code cause")
	invalid := New(Code("MISSING"), true, cause)
	if !errors.Is(invalid, cause) || invalid.Retryable() {
		t.Fatal("invalid code did not preserve cause or disable retry")
	}
}
