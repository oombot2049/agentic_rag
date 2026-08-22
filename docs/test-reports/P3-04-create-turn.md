# P3-04 Create Turn test report

- Date: 2026-08-23
- Commit under test: working tree before P3-04 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Requirement: [0026 Create Turn](../requirements/0026-create-turn.md)

## 1. Result

P3-04 passes command normalization, idempotent replay, REJECT concurrency policy,
typed conflict mapping, trusted-tenant HTTP handling, durable execution options
and dual-toolchain gates. `internal/turn` has 80.0% statement coverage; the HTTP
transport package has 90.6%. The broader repository package is 59.3% because its
Docker-only transaction paths do not run in this Windows sandbox.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/turn ./internal/repository ./internal/transport/http ./migrations` | Pass |
| Coverage | `go test -cover ./internal/turn ./internal/transport/http ./internal/repository` | Pass; 80.0% / 90.6% / 59.3% |
| Repetition | `go test -count=100 ./internal/turn ./internal/transport/http` | Pass; HTTP stress 61.7s |
| Full Go 1.26 | `go test -count=1 ./...`; `go vet ./...`; build | Pass |
| Full Go 1.27 | `go test -count=1 ./...`; build | Pass |
| Static analysis | `staticcheck ./...`; `actionlint` | Pass |
| Patch hygiene | `git diff --check` | Pass |
| Mandatory PostgreSQL | `AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/repository -run TestTurnAPIStoreAtomicConcurrencyAndFaultRollback` | Not executable locally: no Windows Docker provider |
| Race detector | `go test -race ./...` | Not executable locally: no C compiler/CGO; mandatory in Linux CI |

The Docker attempt failed before setup with `rootless Docker is not supported on
Windows`. CI sets `AGENTICRAG_REQUIRE_DOCKER=1`, provides Docker and runs
`go test -race ./...`; these tests cannot silently downgrade to a skip there.

## 3. Command and idempotency evidence

Application tests prove omitted execution options and explicit `max_steps: 12`
produce the same canonical fingerprint and replay the original durable response.
Changed content under the same key returns `IDEMPOTENCY_KEY_REUSED`; live claims
return retryable `IDEMPOTENCY_IN_PROGRESS`. Server IDs, UTC time, path parameters,
subject scope, retention and all validation limits are exercised.

One hundred concurrent copies of the same logical command return one identical
Session/Turn/Run/Message response and the fault model records one physical
create. Stored response status/type/resource references are checked before
replay, so corrupt idempotency data fails closed.

## 4. HTTP and conflict evidence

HTTP tests execute through real authentication and permission middleware. A
spoofed tenant header cannot affect the store. The successful route returns 202,
Run Location, events URL, normalized base version and security/cache headers.
Missing fields, null objects, nested unknown fields, wrong content type, missing
key, trailing JSON and invalid max steps are rejected.

Repository errors map to `SESSION_NOT_FOUND`, `SESSION_VERSION_CONFLICT` and
`SESSION_BUSY` with 404/409 as appropriate. Version mismatch is evaluated before
busy state. A Principal without `sessions:write` receives 403 before application
work.

## 5. Transaction and failure evidence

The mandatory PostgreSQL test applies every migration and submits 20 concurrent
copies of the same command. It asserts exactly one Turn, user Message, Run,
Outbox event and succeeded idempotency record; Session Active Run, next sequence
and row version must match the response. It validates persisted `max_steps` and
the schema-versioned `RunQueued.v1` payload.

Different-key competition must return busy, while a mismatched context returns
version conflict even while busy. For fault injection, a temporary database
constraint rejects only the second Session's Outbox payload—after Session, Turn,
Message and Run writes have been attempted. The command must fail and leave the
second Session at row version 1/sequence 1 with no Active Run and zero child or
idempotency rows, proving transaction-wide rollback.

## 6. Acceptance conclusion

- Atomic Message/Turn/Run/Session/Outbox/idempotency transaction: **mandatory DB test checked in**.
- Same-key and different-key concurrency semantics: **Pass in stress model; DB test checked in**.
- Accurate not-found/version/busy/idempotency codes: **Pass**.
- Trusted tenant and strict HTTP request handling: **Pass**.
- Durable execution options and RunQueued payload: **Pass by static/unit contract; DB test checked in**.
- First-release REJECT policy with no Branch: **Pass**.

P3-04 implementation is complete; container and race execution remain enforced
by the repository's Linux CI gate.
