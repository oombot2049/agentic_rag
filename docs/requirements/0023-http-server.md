# Requirement P3-01: HTTP server lifecycle and middleware

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P1-04 error model, P1-05 application bootstrap
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P3-01

## 1. Objective

Provide the production HTTP process boundary used by later Session, Turn and SSE
handlers. The component owns listener/server lifecycle, deterministic middleware
composition, defensive request-side timeouts and bounded graceful shutdown. It
contains no domain routes or authentication policy.

## 2. Timeout model

| Setting | Default | Purpose |
|---|---:|---|
| read-header timeout | 5s | bound slowloris header delivery |
| request read timeout | 30s | bound headers plus ordinary request bodies |
| idle timeout | 2m | retire inactive keep-alive connections |
| write timeout | **0 / disabled** | preserve long-lived SSE responses |
| shutdown timeout | 15s | owned by application bootstrap |

All configured request timeouts are 100 ms to 5 minutes; idle must not be less
than read-header timeout. `MaxHeaderBytes` is fixed at 1 MiB. Per-route body size,
handler deadlines and SSE heartbeat/write-idle policy belong to later transport
tasks. A global `WriteTimeout` is deliberately forbidden because it starts before
an SSE stream finishes and would sever healthy clients.

## 3. Middleware chain

`Middleware` is `func(http.Handler) http.Handler`. `Chain(final, A, B, C)` makes A
the outermost layer and invokes A → B → C → final on the request path. Nil final
handlers and nil middleware are construction errors; ordering is never inferred
from package registration or global state.

The server adds a final draining gate outside the supplied chain. Once shutdown
begins, requests that have not already entered return `503` with
`Connection: close`; admitted requests continue normally.

## 4. Lifecycle and graceful shutdown

The component implements `app.Component` without importing the app package:
`Name`, `Start`, and `Stop` match structurally. Construction is inactive. Start
binds the listener synchronously so address errors are returned, then serves in a
tracked goroutine. Repeated start and restart-after-stop fail deterministically.

Request base context is derived from an independent service context, not the OS
signal context. Therefore signal cancellation does not cancel in-flight handlers
before `http.Server.Shutdown` has drained them. Stop marks draining, disables
keep-alives, calls bounded `Shutdown`, waits for the serve loop, and cancels the
base context only after graceful completion. If the caller deadline expires it
forces `Close` and joins all errors. Stop before start and repeated Stop are safe.

Unexpected asynchronous serve failure is exposed exactly once through `Errors`;
normal `http.ErrServerClosed` is normalized to nil. `Address` is available only
after a successful start and supports port-zero tests.

## 5. Security and protocol rules

- `http.Server` suppresses the default error logger to avoid raw malformed
  request content entering process logs; structured transport logging comes later.
- no `DefaultServeMux`, global handler registry or implicit middleware exists;
- listener and accepted request contexts are explicit and cancellable;
- HTTP/1 keep-alives stop during drain; HTTP/2 graceful behavior is delegated to
  the standard server implementation;
- TLS termination may be in-process or at a trusted proxy in deployment tasks,
  but clear-text public exposure is not authorized by this component.

## 6. Test strategy

- unit tests verify middleware order, nil rejection and exact server fields;
- real loopback tests verify start, port-zero address and normal serve behavior;
- a blocked handler proves shutdown waits and does not drop an admitted request;
- cancellation simulates the signal path and proves graceful Run termination;
- raw TCP slow-header and idle tests verify configured timeouts;
- concurrent Start/Stop calls run under the Linux race gate.

## 7. Acceptance criteria

- [x] Read, header and idle timeout configuration reaches `http.Server`.
- [x] Global write timeout remains disabled for SSE compatibility.
- [x] Middleware ordering is explicit and deterministic.
- [x] Graceful shutdown drains admitted requests and rejects new work.
- [x] Signal-style cancellation terminates without leaking the serve goroutine.

