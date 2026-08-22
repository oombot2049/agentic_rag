// Package apperror defines stable application error identities and mappings.
package apperror

import "net/http"

// Code is a stable machine-readable error identity.
type Code string

const (
	CodeInvalidArgument        Code = "INVALID_ARGUMENT"
	CodeInvalidIdempotencyKey  Code = "INVALID_IDEMPOTENCY_KEY"
	CodeInvalidEventCursor     Code = "INVALID_EVENT_CURSOR"
	CodeUnauthenticated        Code = "UNAUTHENTICATED"
	CodePermissionDenied       Code = "PERMISSION_DENIED"
	CodeSessionNotFound        Code = "SESSION_NOT_FOUND"
	CodeTurnNotFound           Code = "TURN_NOT_FOUND"
	CodeRunNotFound            Code = "RUN_NOT_FOUND"
	CodeSessionVersionConflict Code = "SESSION_VERSION_CONFLICT"
	CodeSessionBusy            Code = "SESSION_BUSY"
	CodeIdempotencyInProgress  Code = "IDEMPOTENCY_IN_PROGRESS"
	CodeIdempotencyKeyReused   Code = "IDEMPOTENCY_KEY_REUSED"
	CodeRunFenced              Code = "RUN_FENCED"
	CodeEventCursorExpired     Code = "EVENT_CURSOR_EXPIRED"
	CodeResourceDeleted        Code = "RESOURCE_DELETED"
	CodeTenantQuotaExceeded    Code = "TENANT_QUOTA_EXCEEDED"
	CodeGlobalCapacityExceeded Code = "GLOBAL_CAPACITY_EXCEEDED"
	CodeInternal               Code = "INTERNAL"
	CodeProviderUnavailable    Code = "PROVIDER_UNAVAILABLE"
	CodeRetrievalUnavailable   Code = "RETRIEVAL_UNAVAILABLE"
	CodeServiceUnavailable     Code = "SERVICE_UNAVAILABLE"
	CodeRunDeadlineExceeded    Code = "RUN_DEADLINE_EXCEEDED"
	CodeProviderTimeout        Code = "PROVIDER_TIMEOUT"
)

type specification struct {
	status  int
	message string
}

var specifications = map[Code]specification{
	CodeInvalidArgument:        {http.StatusBadRequest, "The request is invalid."},
	CodeInvalidIdempotencyKey:  {http.StatusBadRequest, "The idempotency key is invalid."},
	CodeInvalidEventCursor:     {http.StatusBadRequest, "The event cursor is invalid."},
	CodeUnauthenticated:        {http.StatusUnauthorized, "Authentication is required."},
	CodePermissionDenied:       {http.StatusForbidden, "Permission is denied."},
	CodeSessionNotFound:        {http.StatusNotFound, "The session was not found."},
	CodeTurnNotFound:           {http.StatusNotFound, "The turn was not found."},
	CodeRunNotFound:            {http.StatusNotFound, "The run was not found."},
	CodeSessionVersionConflict: {http.StatusConflict, "The session context has advanced."},
	CodeSessionBusy:            {http.StatusConflict, "The session already has an active run."},
	CodeIdempotencyInProgress:  {http.StatusConflict, "The request is already being processed."},
	CodeIdempotencyKeyReused:   {http.StatusConflict, "The idempotency key was used for different input."},
	CodeRunFenced:              {http.StatusConflict, "The run execution lease is stale."},
	CodeEventCursorExpired:     {http.StatusGone, "The event cursor has expired."},
	CodeResourceDeleted:        {http.StatusGone, "The resource has been deleted."},
	CodeTenantQuotaExceeded:    {http.StatusTooManyRequests, "The tenant quota has been exceeded."},
	CodeGlobalCapacityExceeded: {http.StatusTooManyRequests, "The service is at capacity."},
	CodeInternal:               {http.StatusInternalServerError, "An internal error occurred."},
	CodeProviderUnavailable:    {http.StatusBadGateway, "The model provider is unavailable."},
	CodeRetrievalUnavailable:   {http.StatusServiceUnavailable, "Retrieval is unavailable."},
	CodeServiceUnavailable:     {http.StatusServiceUnavailable, "The service is unavailable."},
	CodeRunDeadlineExceeded:    {http.StatusGatewayTimeout, "The run deadline was exceeded."},
	CodeProviderTimeout:        {http.StatusGatewayTimeout, "The model provider timed out."},
}

var orderedCodes = []Code{
	CodeInvalidArgument, CodeInvalidIdempotencyKey, CodeInvalidEventCursor,
	CodeUnauthenticated, CodePermissionDenied,
	CodeSessionNotFound, CodeTurnNotFound, CodeRunNotFound,
	CodeSessionVersionConflict, CodeSessionBusy, CodeIdempotencyInProgress,
	CodeIdempotencyKeyReused, CodeRunFenced,
	CodeEventCursorExpired, CodeResourceDeleted,
	CodeTenantQuotaExceeded, CodeGlobalCapacityExceeded,
	CodeInternal, CodeProviderUnavailable, CodeRetrievalUnavailable,
	CodeServiceUnavailable, CodeRunDeadlineExceeded, CodeProviderTimeout,
}

// Valid reports whether the code is part of the public registry.
func (c Code) Valid() bool {
	_, ok := specifications[c]
	return ok
}

// Codes returns all registered codes in stable contract order.
func Codes() []Code {
	return append([]Code(nil), orderedCodes...)
}
