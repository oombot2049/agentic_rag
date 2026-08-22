# P1-07 Redis/Valkey foundation test report

- Date: 2026-08-23
- Toolchain: Go 1.27.0 windows/amd64
- Dependencies: go-redis/v9.22.0; Valkey 9.1 reference
- Requirement: [0013 Redis foundation](../requirements/0013-redis-foundation.md)

## Result

Key isolation, configuration, TTL guard, password non-disclosure, fuzz,
regression, vet and build gates pass. The Valkey Testcontainers test compiled
and was invoked, but explicitly **SKIPPED** because Docker is unavailable on this
host. P1-08 must make this test non-skippable in Linux CI.

| Gate | Result |
|---|---|
| `go test -v -cover ./internal/platform/redis` | Pass with one Docker skip; hermetic coverage 66.2% |
| 50 randomized repetitions | Pass |
| 5-second native Go fuzz | Pass; 46,247 executions, 31 new interesting inputs |
| 5,000 deterministic randomized keys | Pass; no cross-tenant collision |
| `go test -p=1 ./...` | Pass, all packages |
| `go vet ./...` | Pass |
| reproducible executable build | Pass |
| `git diff --check` | Pass |
| Valkey 9.1 real integration | Not executed: Docker unavailable |

The first parallel full build stalled in one Windows compiler subprocess with
no CPU progress. It was interrupted and rerun serially with `-p=1`; every package
then completed. This is recorded as a host/tooling incident, not a product test
failure or skipped package.

## Verified behavior

- environment, tenant, namespace and logical segment are mandatory;
- delimiter, brace, wildcard, whitespace and path-style injections reject;
- different tenants cannot produce the same encoded logical key;
- only the closed namespace registry is accepted;
- zero, negative and over-cap TTLs reject before any network call;
- empty typed keys reject for Set/Get/Delete;
- a connection failure containing a marker password returns a safe operation
  error without the password;
- Redis pool/timeout/TTL configuration boundaries and min-idle ≤ pool-size are
  table-tested.

When Docker is available, the compiled integration test verifies Ping, bounded
Set/Get, expiration to `ErrMiss`, Delete and cleanup against Valkey 9.1.

## Acceptance

- Timeouts, pool configuration and health implementation: **Pass locally**.
- Tenant-prefixed typed Key contract: **Pass locally**.
- TTL/persistence guard and secret-safe failures: **Pass locally**.
- Real Valkey behavior: **Pending mandatory P1-08 Docker CI**.

P1-07 implementation is complete with the environment-dependent verification
gap explicitly retained as a CI gate.
