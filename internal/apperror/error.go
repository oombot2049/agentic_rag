package apperror

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is a safe application error with an optional private cause.
type Error struct {
	code      Code
	retryable bool
	cause     error
}

// New constructs an error occurrence. Retryability must be chosen by the
// boundary that observed the failure. Invalid codes safely become INTERNAL.
func New(code Code, retryable bool, cause error) *Error {
	if !code.Valid() {
		if cause == nil {
			cause = fmt.Errorf("unknown application error code %q", code)
		}
		code = CodeInternal
		retryable = false
	}
	return &Error{code: code, retryable: retryable, cause: cause}
}

// Code returns the stable machine identity.
func (e *Error) Code() Code {
	if e == nil || !e.code.Valid() {
		return CodeInternal
	}
	return e.code
}

// Retryable reports the occurrence-specific retry decision.
func (e *Error) Retryable() bool {
	return e != nil && e.retryable
}

// HTTPStatus returns the centralized transport mapping.
func (e *Error) HTTPStatus() int {
	return specificationFor(e.Code()).status
}

// PublicMessage returns the repository-owned safe message.
func (e *Error) PublicMessage() string {
	return specificationFor(e.Code()).message
}

// Error deliberately omits the internal cause.
func (e *Error) Error() string {
	return string(e.Code()) + ": " + e.PublicMessage()
}

// Unwrap exposes the cause to errors.Is/As without formatting it.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is treats a stable code as application-error identity. The standard library
// continues along Unwrap for cause identity.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e != nil && e.Code() == other.Code()
}

func specificationFor(code Code) specification {
	if spec, ok := specifications[code]; ok {
		return spec
	}
	return specifications[CodeInternal]
}

// PublicError is the safe error portion consumed by transports. Request and
// trace identifiers are added by the transport envelope.
type PublicError struct {
	Code      Code   `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Normalize preserves an application error through wrapping or converts an
// unknown error to INTERNAL. Nil remains nil.
func Normalize(err error) *Error {
	if err == nil {
		return nil
	}
	var applicationError *Error
	if errors.As(err, &applicationError) {
		return applicationError
	}
	return New(CodeInternal, false, err)
}

// Public returns a cause-free view. A nil input safely maps to INTERNAL so a
// transport bug cannot emit an empty/invalid error object.
func Public(err error) PublicError {
	normalized := Normalize(err)
	if normalized == nil {
		normalized = New(CodeInternal, false, nil)
	}
	return PublicError{
		Code: normalized.Code(), Message: normalized.PublicMessage(), Retryable: normalized.Retryable(),
	}
}

// HTTPStatus returns the status for an error, with unknown and nil values
// falling back to 500.
func HTTPStatus(err error) int {
	if normalized := Normalize(err); normalized != nil {
		return normalized.HTTPStatus()
	}
	return http.StatusInternalServerError
}

// IsCode matches a registered code anywhere in an error chain.
func IsCode(err error, code Code) bool {
	return code.Valid() && errors.Is(err, New(code, false, nil))
}
