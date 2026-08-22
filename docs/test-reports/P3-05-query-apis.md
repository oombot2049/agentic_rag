# P3-05 query APIs test report

- Date: 2026-08-23
- Commit under test: working tree before P3-05 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Requirement: [0027 query APIs](../requirements/0027-query-apis.md)

## 1. Result

P3-05 passes tenant-scoped Turn/Run projection, selected-message privacy,
conversation-history pagination, authenticated cursor and HTTP non-enumeration
tests. `internal/turn` has 83.8% statement coverage and the HTTP transport package
has 90.8%.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./...` | Pass |
| Coverage | `go test -cover ./internal/turn ./internal/transport/http ./internal/repository` | Pass; 83.8% / 90.8% / 57.4% |
| Repetition | `go test -count=100 ./internal/turn ./internal/transport/http` | Pass; HTTP stress 54.8s |
| Full Go 1.26 | `go test -count=1 ./...`; `go vet ./...` | Pass |
| Full Go 1.27 | `go test -count=1 ./...`; build | Pass |
| Static analysis | `staticcheck ./...`; `actionlint` | Pass |
| Patch hygiene | `git diff --check` | Pass |
| PostgreSQL pagination | `AGENTICRAG_REQUIRE_DOCKER=1 go test ./internal/platform/postgres -run TestConversationRepositoryTenantIsolationTransactionsAndPagination` | Not executable locally: no Windows Docker provider |
| Race detector | `go test -race ./...` | Not executable locally: no C compiler/CGO; mandatory in Linux CI |

The explicit container attempt failed before setup because rootless Docker is not
supported by the Windows provider. Linux CI supplies Docker, sets
`AGENTICRAG_REQUIRE_DOCKER=1`, and runs the complete race suite.

## 3. Cursor and pagination evidence

Cursor tests round-trip an equal-timestamp `(created_at,message_id)` key in UTC.
They mutate the caller-owned key after codec construction to prove defensive
copying, then reject a changed byte, non-canonical padding, cross-Session use and
cross-tenant use. Short signing keys fail construction.

Service and HTTP tests page two Messages with the exact same timestamp at size
one. The second page contains a different ID with no duplicate/gap and no third
cursor. Missing page size defaults to 50; explicit 0, values above 100,
non-numeric, empty, duplicate and unknown query fields fail with 400.

The repository query adapter requires a read-only Repeatable Read transaction
for each query. Turn hydration and its ordered Run-ID query, and Session
existence plus message-page query, therefore share one database snapshot.

## 4. Visibility and projection evidence

Turn projection includes ordered candidates, selected Run and terminal update
time. Run projection includes only durable public fields. A selected Run exposes
its final Message ID; a superseded Run with an output candidate explicitly
returns `final_message_id: null`. Worker lease, token, execution version and
checkpoint never enter the view types.

The query service rejects nil or non-CONVERSATION Messages even if a faulty store
returns them. The SQL repository independently filters
`visibility='CONVERSATION'`; its mandatory PostgreSQL test inserts candidate and
visible assistant Messages and expects only the user and promoted answer across
pages.

## 5. HTTP isolation evidence

Tests run all routes through real authentication and `sessions:read` permission
middleware. Spoofed tenant headers are stripped and the store observes only
`ten_0001`. Turn, Run and history responses carry safe JSON/no-store/nosniff
headers. A foreign-owned Turn and an absent Turn produce byte-identical 404
bodies; missing permission returns 403 before store access.

## 6. Acceptance conclusion

- Stable deterministic pagination: **Pass**.
- Opaque authenticated ownership-bound cursor: **Pass**.
- Only selected/final assistant Message is exposed: **Pass**.
- Tenant-scoped Turn/Run/history reads and non-enumerable errors: **Pass**.
- PostgreSQL candidate exclusion/equal-time pagination: **mandatory CI test already checked in**.

P3-05 is complete; Docker and race execution remain enforced by Linux CI.
