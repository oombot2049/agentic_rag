# Requirement P1-07: Redis/Valkey transient foundation

- Status: Implemented
- Priority: P1
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P1-02 configuration](0008-typed-configuration.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P1-07

## 1. Decision

Use `go-redis/v9.22.0` against the tested Redis-protocol subset, with Valkey 9.1
as reference server. The adapter is transient infrastructure only: PostgreSQL
remains authoritative for conversations, idempotency and durable events.

## 2. Key safety contract

All keys are created by `KeyBuilder`; raw string keys are not accepted by client
methods. Format:

```text
ar:<environment>:{<tenant_id>}:<namespace>:<logical-segment>[:...]
```

Environment prevents deployment collision; tenant is mandatory and enclosed in
a cluster hash tag; namespace is a closed enum; every segment is 1–128 ASCII
letters/digits/underscore/hyphen. Colons, braces, whitespace, wildcards and path
characters are rejected. Total encoded length is at most 512 bytes. This makes
tenant omission and delimiter injection impossible by construction.

Initial namespaces are cache, rate, events, lock and idempotency. Adding one is
a reviewed code change with TTL/fallback ownership.

## 3. Client behavior

`Open` installs typed dial/read/write/pool timeouts, pool size/min-idle settings,
reveals the password only in client construction and performs a bounded Ping.
Errors format only a safe operation label while retaining the cause via Unwrap.
`Health` has an independent timeout.

`Set` requires an explicit positive TTL no greater than the configured 24-hour
cap. Persistent writes are rejected. `Get` converts protocol nil into stable
`ErrMiss`; `Delete` accepts typed keys only. Payload schema/checksum validation
belongs to the cache value codec of the owning feature, not this transport.

## 4. Failure policy

Cache failure is never interpreted as a hit and never broadens tenant scope.
Owning application code either falls back to PostgreSQL or returns its defined
availability error. No retry loop is hidden in this wrapper beyond the client's
bounded connection behavior. Close is explicit through application lifecycle.

## 5. Verification

Table, property-style randomized and fuzz tests cover key construction,
cross-tenant inequality and injection rejection. Client tests cover TTL bounds
and secret-safe errors. Testcontainers against Valkey verifies Ping, Set/Get,
expiration, miss, Delete and pool concurrency when Docker is available; P1-08
makes that Linux Docker gate mandatory.

## 6. Acceptance

- [x] Dial, command, pool and health timeouts are typed and bounded.
- [x] Pool size/min-idle and password are configured without log disclosure.
- [x] Every client operation requires a tenant-scoped typed Key.
- [x] Persistent Set and TTL beyond retention policy are rejected.
- [x] Key isolation tests and real Valkey integration tests are present.
- [x] Redis/Valkey is explicitly non-authoritative.
