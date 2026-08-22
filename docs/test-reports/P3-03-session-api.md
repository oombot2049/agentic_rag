# P3-03 Session API test report

- Date: 2026-08-23
- Commit under test: working tree before P3-03 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Requirement: [0025 Session API](../requirements/0025-session-api.md)

## 1. Result

P3-03 passes application and HTTP tests for tenant-scoped create/get, strict
validation, immutable audit fields, context/row versions, idempotent replay,
concurrent duplicate submission, stable errors and cross-tenant
non-enumeration. Coverage is 80.1% for `internal/session` and 89.5% for the HTTP
transport package (which includes the earlier server lifecycle).

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/session ./internal/repository ./internal/transport/http ./migrations` | Pass |
| Coverage | `go test -cover ./internal/session ./internal/transport/http ./internal/repository` | Pass; 80.1% / 89.5% / 69.4% |
| Repetition | `go test -count=100 ./internal/session ./internal/transport/http` | Pass; 100 repetitions |
| Full Go 1.27 | `GOTOOLCHAIN=local go test ./...` | Pass |
| Full Go 1.26 | `GOTOOLCHAIN=local go test ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...` | Pass |
| Patch hygiene | `git diff --check` | Pass |
| PostgreSQL mandatory test | `AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/repository -run TestSessionAPIStoreAtomicConcurrentCreateAndTenantIsolation` | Not executable locally: no Windows Docker provider |
| Race detector | `go test -race ...` | Not executable locally: no C compiler/CGO; enforced by Linux CI |

The two unavailable local gates are not silently skipped: the checked-in CI
sets `AGENTICRAG_REQUIRE_DOCKER=1` and executes `go test -race ./...` on Ubuntu,
where Docker and CGO are available. A local mandatory Docker attempt failed
before test setup with `rootless Docker is not supported on Windows`.

## 3. Application and idempotency evidence

Service tests inject deterministic IDs and clocks and verify initial versions
(`context_version=0`, `row_version=1`, next sequence 1), UTC audit timestamps,
canonical empty metadata, minimum idempotency retention and the persisted 201
resource reference. One hundred concurrent goroutines using the same scope/key
receive one logical Session ID and produce one physical store create.

Changed input under the same key maps to `IDEMPOTENCY_KEY_REUSED`; a live claim
maps to retryable `IDEMPOTENCY_IN_PROGRESS`. Completed replay is reconstructed
from the durable original response rather than the current mutable aggregate, so
a later Session mutation cannot change the response to the original command.

## 4. HTTP and isolation evidence

HTTP tests run through the real authentication and permission middleware. A
spoofed `X-Tenant-ID` is stripped and the store observes only the verifier's
`ten_0001`. Create returns 201, Location, `"rv-1"` ETag, JSON and security/cache
headers. GET returns the same public representation and ETag.

Table tests reject missing/wrong content type, empty/null/oversized bodies,
unknown fields, trailing JSON, short keys and invalid metadata. Read-only
Principals cannot create. A Session present only under tenant B and an absent ID
produce byte-identical 404 bodies for tenant A.

## 5. PostgreSQL contract evidence

The integration test provisions PostgreSQL 18, applies every embedded migration
and starts 20 concurrent creates against the real transaction adapter. It
requires one Session row, one succeeded idempotency row, retained JSONB metadata,
one returned ID, changed-input rejection, tenant-scoped not-found and successful
owner GET. The test is mandatory in CI and cannot downgrade an unavailable
container runtime to a skip there.

Static migration tests additionally require paired up/down files, JSON object
shape, a maximum of 20 properties and a narrow rollback that drops only the new
column.

## 6. Acceptance conclusion

- Tenant isolation and non-enumeration: **Pass in unit/HTTP; mandatory DB test checked in**.
- Parameter and body validation: **Pass**.
- Server-controlled audit fields and initial versions: **Pass**.
- `context_version`, Location and ETag response contract: **Pass**.
- Atomic concurrent idempotency: **Pass in stress model; mandatory PostgreSQL test checked in**.

P3-03 implementation is complete; Docker/race execution remains an explicit CI
gate because this Windows sandbox has neither a working Docker provider nor CGO
compiler.
