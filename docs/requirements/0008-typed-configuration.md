# Requirement P1-02: Typed layered configuration

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P1-01 Go foundation](0007-go-project-foundation.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P1-02

## 1. Objective

Provide one typed, validated and secret-safe configuration boundary for the
process. A malformed deployment must fail before listeners, database pools or
workers start. Application packages consume `config.Config`; they do not read
environment variables independently.

## 2. Loading and precedence

`LoadSources` begins with repository-owned defaults and overlays each `Source`
from left to right. A key absent from a source preserves the previous value; a
present empty value is intentional and is validated. `Load` uses the process
environment as its only overlay.

```text
compiled defaults < source[0] < ... < source[n]
```

`MapSource` exists for deterministic tests and future composition. Adding a
config file, secret manager or remote provider requires a separate adapter; it
must use the same keys, precedence and validation path. Runtime reload is not
supported: a changed configuration requires a rolling restart.

## 3. Typed contract

| Group | Key | Type/default | Constraint |
|---|---|---|---|
| process | `AGENTICRAG_ENVIRONMENT` | enum/`development` | development, test, production |
| server | `AGENTICRAG_SERVER_ADDRESS` | string/`:8080` | valid TCP host:port |
| server | `AGENTICRAG_SERVER_READ_TIMEOUT` | duration/`30s` | 100ms–5m |
| server | `AGENTICRAG_SERVER_READ_HEADER_TIMEOUT` | duration/`5s` | 100ms–30s |
| server | `AGENTICRAG_SERVER_IDLE_TIMEOUT` | duration/`2m` | 100ms–5m; ≥ header timeout |
| server | `AGENTICRAG_SERVER_SHUTDOWN_TIMEOUT` | duration/`15s` | 1s–2m |
| PostgreSQL | `AGENTICRAG_POSTGRES_DSN` | secret/required | non-empty |
| PostgreSQL | `AGENTICRAG_POSTGRES_MAX_OPEN_CONNS` | integer/50 | 1–500 |
| PostgreSQL | `AGENTICRAG_POSTGRES_MAX_IDLE_CONNS` | integer/10 | 0–max open |
| PostgreSQL | `AGENTICRAG_POSTGRES_CONN_MAX_LIFETIME` | duration/30m | 1m–24h |
| PostgreSQL | `AGENTICRAG_POSTGRES_CONNECT_TIMEOUT` | duration/5s | 100ms–1m |
| PostgreSQL | `AGENTICRAG_POSTGRES_HEALTH_TIMEOUT` | duration/2s | 100ms–30s |
| Redis | `AGENTICRAG_REDIS_ADDRESS` | string/`127.0.0.1:6379` | valid TCP host:port |
| Redis | `AGENTICRAG_REDIS_PASSWORD` | secret/empty | optional |
| Redis | `AGENTICRAG_REDIS_DB` | integer/0 | 0–15 |
| Redis | `AGENTICRAG_REDIS_DIAL_TIMEOUT` | duration/5s | 100ms–1m |
| Redis | `AGENTICRAG_REDIS_READ_TIMEOUT` | duration/2s | 100ms–30s |
| Redis | `AGENTICRAG_REDIS_WRITE_TIMEOUT` | duration/2s | 100ms–30s |
| Redis | `AGENTICRAG_REDIS_POOL_TIMEOUT` | duration/2s | 100ms–1m |
| Redis | `AGENTICRAG_REDIS_HEALTH_TIMEOUT` | duration/2s | 100ms–30s |
| Redis | `AGENTICRAG_REDIS_POOL_SIZE` | integer/100 | 1–1000 |
| Redis | `AGENTICRAG_REDIS_MIN_IDLE_CONNS` | integer/5 | 0–pool size |
| Redis | `AGENTICRAG_REDIS_MAX_TTL` | duration/24h | 1s–24h |
| queue | `AGENTICRAG_QUEUE_MAX_PENDING_PER_TENANT` | integer/250 | 1–100,000 |
| queue | `AGENTICRAG_QUEUE_MAX_PENDING_GLOBAL` | integer/6,000 | per-tenant–1,000,000 |
| worker | `AGENTICRAG_WORKER_ID` | string/`worker-1` | safe text, 1–256 bytes |
| worker | `AGENTICRAG_WORKER_MAX_CONCURRENCY` | integer/32 | 1–1,000 |
| worker | `AGENTICRAG_WORKER_MAX_CONCURRENCY_PER_TENANT` | integer/4 | 1–global concurrency |
| worker | `AGENTICRAG_WORKER_POLL_INTERVAL` | duration/`100ms` | 1ms–1m |
| worker | `AGENTICRAG_WORKER_LEASE_DURATION` | duration/`30s` | 5s–5m |
| worker | `AGENTICRAG_WORKER_RENEW_INTERVAL` | duration/`10s` | 1ms–lease duration (exclusive) |
| worker | `AGENTICRAG_WORKER_RETRY_DELAY` | duration/`1s` | 0–24h |
| logging | `AGENTICRAG_LOG_LEVEL` | enum/`info` | debug, info, warn, error |

These are process/bootstrap values, not tenant policy. Capacity and Agent limits
will extend the same typed contract in their owning requirements.

## 4. Validation behavior

Loading parses all known keys, then validates the resulting object. Problems are
returned together in deterministic key order as `ValidationError`; callers can
use `errors.As`. Error messages contain a key and a rule, never the supplied
value. Cross-field validation enforces idle connections not exceeding open
connections.

Unknown environment variables are ignored because a process can contain
unrelated variables. Misspelled `AGENTICRAG_` keys are addressed by deployment
schema/CI once manifests exist; silently guessing a replacement is prohibited.

## 5. Secret boundary

Secrets use `Secret`, whose underlying value is private. `String`, `GoString`,
`Format`, text marshaling and JSON marshaling all produce `[REDACTED]`. Reading a
secret requires the explicit `Reveal` method at the adapter that needs it.
`Config.SafeFields` is an allowlist for structured startup logs and cannot
contain DSN or password fields. Config errors likewise never interpolate raw
input.

The type prevents common accidental logging; it cannot protect a caller that
explicitly invokes `Reveal`. Code review must keep that call at infrastructure
client construction boundaries.

## 6. Package/API design

```text
internal/config
  Config and nested value objects
  Source, EnvironmentSource, MapSource
  Load / LoadSources
  ValidationError and FieldError
  Secret
```

The package uses only the standard library. It performs no I/O besides reading
the process environment and has no global mutable configuration singleton.

## 7. Test strategy

Table-driven tests cover defaults/overlay precedence, required and malformed
values, enum and numeric boundaries, cross-field rules, stable multi-error
ordering, environment lookup, safe-field allowlisting and secret rendering
through `fmt` and JSON. Quality gates remain `gofmt`, `go test`, `go vet` and a
full build.

## 8. Acceptance criteria

- [x] Missing required configuration fails loading before bootstrap.
- [x] Defaults and ordered overlays produce a typed immutable-by-convention value.
- [x] Invalid scalar and cross-field values return deterministic field errors.
- [x] Sensitive values do not appear in formatting, JSON, safe fields or errors.
- [x] Table-driven unit tests cover valid, invalid and boundary cases.
- [x] No package outside `internal/config` needs direct environment access.

## 9. Follow-up ownership

P1-03 consumes `SafeFields` for structured logging. P1-05 calls `Load` before
constructing dependencies. P1-06/P1-07 reveal connection secrets only while
building PostgreSQL/Redis clients. Deployment manifest validation is owned by
P1-08 and release tasks.
