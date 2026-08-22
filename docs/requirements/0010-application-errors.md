# Requirement P1-04: Application error model and HTTP mapping

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P0 API contract](../api/0005-domain-and-api-contract.md), [P1-01 Go foundation](0007-go-project-foundation.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P1-04

## 1. Objective

Give domain, application and transport code one stable error identity without
coupling business packages to HTTP or exposing SQL/provider/tool failure text.
An occurrence has a stable code, repository-owned public message, explicit
retry decision, HTTP mapping and optional internal cause.

## 2. Separation of concerns

```text
internal cause ──Unwrap──> app error(code, retryable)
                              │
                              ├── logs/metrics: code + correlation
                              └── transport: PublicError + request/trace IDs
```

- `Code` is the machine-stable identity and the only programmatic API contract.
- Public messages are fixed in the code registry. Constructors cannot accept an
  arbitrary client-facing string.
- `retryable` is mandatory at construction and varies by occurrence; it is not
  inferred from HTTP status.
- The cause remains reachable by `errors.Is/As` and `Unwrap`, but `Error()` and
  `Public()` never render it.
- HTTP mapping is centralized in this package so handlers cannot disagree.
- Request/trace IDs and optional documented details are transport concerns and
  are added in P3; they are not stored inside reusable domain errors.

## 3. Stable code registry

| HTTP | Codes |
|---:|---|
| 400 | `INVALID_ARGUMENT`, `INVALID_IDEMPOTENCY_KEY`, `INVALID_EVENT_CURSOR` |
| 401 | `UNAUTHENTICATED` |
| 403 | `PERMISSION_DENIED` |
| 404 | `SESSION_NOT_FOUND`, `TURN_NOT_FOUND`, `RUN_NOT_FOUND` |
| 409 | `SESSION_VERSION_CONFLICT`, `SESSION_BUSY`, `IDEMPOTENCY_IN_PROGRESS`, `IDEMPOTENCY_KEY_REUSED`, `RUN_FENCED` |
| 410 | `EVENT_CURSOR_EXPIRED`, `RESOURCE_DELETED` |
| 429 | `TENANT_QUOTA_EXCEEDED`, `GLOBAL_CAPACITY_EXCEEDED` |
| 500 | `INTERNAL` |
| 502 | `PROVIDER_UNAVAILABLE` |
| 503 | `RETRIEVAL_UNAVAILABLE`, `SERVICE_UNAVAILABLE` |
| 504 | `RUN_DEADLINE_EXCEEDED`, `PROVIDER_TIMEOUT` |

Public messages describe a safe recovery category and never contain resource
values. New codes require API review, a registry entry and mapping tests.
Unknown or invalid codes normalize to `INTERNAL`.

## 4. Go API and matching

```go
err := apperror.New(apperror.CodeSessionBusy, true, repositoryErr)
errors.Is(err, repositoryErr)                         // true
errors.Is(err, apperror.New(apperror.CodeSessionBusy, false, nil)) // true by code
apperror.IsCode(err, apperror.CodeSessionBusy)       // true through wrapping
public := apperror.Public(err)                        // no cause
```

`errors.As` obtains `*apperror.Error`. `errors.Is` matches either a wrapped
cause or another application error with the same code; retryability is occurrence
metadata and is deliberately excluded from identity. `Normalize` preserves an
existing application error found through wrappers and converts any other error
to `INTERNAL`.

## 5. Safety invariants

1. `Error()` contains only code and registered public message.
2. `Public()` returns only code, registered message and retryability.
3. An internal cause is never formatted implicitly.
4. Unknown codes do not become public codes.
5. Nil is not an error: `Normalize(nil)` returns nil.
6. Transport handlers must use `Public`, never `err.Error()` as response text.
7. Prompt/tool/provider/SQL detail belongs in sanitized internal telemetry, not
   the error envelope.

## 6. Test strategy

Table tests cover every registered code's HTTP status and public fields, code
syntax/uniqueness, explicit retryability, same-code `errors.Is`, cause-chain
`errors.Is/As`, unknown-cause normalization, invalid-code fallback, nil behavior
and marker-secret non-disclosure through Error/Public formatting.

## 7. Acceptance criteria

- [x] Every P0 core error code has one stable HTTP mapping and public message.
- [x] Retryability is explicit per occurrence, not status-derived.
- [x] Internal causes remain available to `errors.Is/As` but never enter public output.
- [x] Unknown errors and invalid codes map safely to `INTERNAL`.
- [x] Table-driven tests cover the complete registry and wrapping behavior.

## 8. Follow-up ownership

P2 repositories translate constraint/not-found/fencing failures into these
codes. P3 transport builds the versioned envelope and adds request/trace IDs plus
allowlisted typed details. P4/P5/P6 classify provider, retrieval and tool
failures at their owning boundaries. P7 reports code/retryability metrics.
