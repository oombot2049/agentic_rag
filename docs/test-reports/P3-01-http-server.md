# P3-01 HTTP server test report

- Date: 2026-08-23
- Commit under test: working tree before P3-01 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Requirement: [0023 HTTP server](../requirements/0023-http-server.md)

## 1. Result

P3-01 passes HTTP configuration, middleware ordering, real loopback timeout,
graceful drain, forced-close, signal-style cancellation, lifecycle concurrency
and dual-toolchain quality gates. `internal/transport/http` has 92.0% statement
coverage; the extended typed configuration package remains at 97.1%.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/config ./internal/transport/http` | Pass |
| Coverage | `go test -cover ./internal/transport/http ./internal/config` | Pass; 92.0% / 97.1% |
| Randomized repetition | `go test -shuffle=on -count=100 ./internal/transport/http ./internal/config` | Pass; 73.2s HTTP stress run |
| Full Go 1.27 | `go test -count=1 ./...`; `go vet ./...`; reproducible build | Pass |
| Full Go 1.26 | `GOTOOLCHAIN=local go test -count=1 ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...`; `actionlint` | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Timeout evidence

Construction tests inspect the actual `http.Server` and require configured
`ReadTimeout`, `ReadHeaderTimeout`, `IdleTimeout`, 1 MiB `MaxHeaderBytes`, and
exactly zero `WriteTimeout` for SSE compatibility.

Real TCP tests then prove behavior rather than configuration alone:

- a partial slow Header is closed after the header deadline;
- a keep-alive connection cannot be reused after the idle deadline;
- a body shorter than its declared `Content-Length` is interrupted by the read
  deadline and the handler observes the timeout;
- all cases use loopback listeners and raw HTTP/1.1 framing, not mocked clocks.

## 4. Graceful lifecycle evidence

A handler blocks after admission. Concurrent Stop is started and is required not
to return during the observation window. After release, the client receives the
complete response and Stop succeeds. Ten concurrent Stop callers share the same
completion without duplicate close behavior.

Cancellation of `Run` models the OS-signal path and must terminate within two
seconds. A separate deadline test leaves a handler blocked until request context
cancellation; bounded Shutdown expires, forces Close, cancels the handler and
returns `context.DeadlineExceeded` rather than hanging.

Direct listener failure is injected and must be exposed via `Errors` and returned
from Stop. Start-twice, restart-after-stop, pre-start Stop, nil contexts, invalid
configuration and address-before-start are deterministic.

## 5. Middleware and draining evidence

The chain test verifies exact request/response order:
`A before → B before → handler → B after → A after`. Nil handler, nil middleware
and middleware returning nil all fail construction. When draining is set, the
outer gate returns 503 plus `Connection: close` without invoking application
middleware or the handler.

## 6. Configuration evidence

Typed configuration now includes read and idle timeouts in addition to the
existing header and shutdown values. Parsing, min/max boundaries, safe startup
fields and the cross-field rule `idle >= read-header` are tested. Error messages
continue to contain keys/rules only, never supplied values.

## 7. Acceptance conclusion

- Read/header/idle timeouts are configured and behaviorally effective: **Pass**.
- Global write timeout cannot truncate SSE: **Pass; fixed at zero**.
- Middleware composition is explicit and stable: **Pass**.
- Graceful shutdown preserves admitted requests: **Pass**.
- Signal cancellation, failure and forced-close paths terminate cleanly: **Pass**.

P3-01 is complete.
