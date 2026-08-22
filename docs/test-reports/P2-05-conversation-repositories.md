# P2-05 conversation repositories test report

- Date: 2026-08-23
- Commit under test: working tree before P2-05 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Database target: PostgreSQL 18.6 in mandatory Linux CI
- Requirement: [0019 conversation repositories](../requirements/0019-conversation-repositories.md)

## 1. Result

P2-05 passes tenant-scope, write mapping, hydration, pagination, transaction,
full-repository and static-analysis gates. `internal/repository` has 81.5%
statement coverage. The real PostgreSQL tenant-isolation/rollback test is checked
in; local Docker absence is explicit and mandatory mode fails rather than skips.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/repository ./internal/platform/postgres` | Pass |
| Unit + SQL coverage | `go test -cover ./internal/repository` | Pass; 81.5% |
| Randomized repetition | `go test -shuffle=on -count=100 ./internal/repository` | Pass |
| PostgreSQL package | `go test -cover ./internal/platform/postgres` | Pass; Docker tests explicitly skipped locally |
| Mandatory provider behavior | `AGENTICRAG_REQUIRE_DOCKER=1 go test -run '^TestConversationRepositoryTenantIsolationTransactionsAndPagination$' ./internal/platform/postgres` | Expected non-zero without Docker |
| Full Go 1.27 regression | `go test -count=1 ./...` | Pass |
| Go 1.27 vet/build | `go vet ./...`; `go build -buildvcs=false -trimpath ...` | Pass |
| Full Go 1.26 regression | `GOTOOLCHAIN=local go test -count=1 ./...` | Pass |
| Go 1.26 vet | `GOTOOLCHAIN=local go vet ./...` | Pass |
| Staticcheck | `staticcheck ./...` with Go 1.26.7 | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Repository evidence

Scripted SQL tests exercise actual read queries and capture their arguments:

- Session, Turn, Message and history calls all contain `tenant_id`, with tenant
  as the first bound argument;
- Turn hydration loads Run IDs separately in `run_no` order and validates the
  reconstructed aggregate;
- Message hydration validates tenant/parent IDs, completed status, role and
  visibility through `domain.RestoreMessage`;
- missing rows map to one `ErrNotFound` without revealing foreign-tenant existence;
- history SQL filters `visibility='CONVERSATION'`, orders by `created_at,id`,
  requests `limit+1` and emits a next cursor at the last returned item;
- nil aggregates/executor, invalid IDs/cursors/limits, corrupt rows and database
  errors fail without mutation or unsafe fallback.

Insert tests verify tenant and resource ID are the first two parameters and
optional Run/head/selection references become SQL null rather than empty IDs.

## 4. Real PostgreSQL suite specification

The integration test applies all migrations and stores the same Session, Turn,
Message and Run IDs for two tenants with different content. Repository reads
return only the scoped tenant's content and a third tenant receives not found.

It inserts an assistant candidate and a committed assistant Message at the same
timestamp, then pages with limit one. The candidate is absent; the `(time,id)`
tie-breaker returns user then committed assistant exactly once with no gap.

A caller-owned transaction inserts Session, Turn, Message and Run through a
transaction-bound repository, deliberately returns an error and proves all rows
roll back. The same workflow then commits for another tenant and hydrates the
Turn with its Run. The repository never starts or commits either transaction.

## 5. Acceptance conclusion

- Every repository operation requires tenant scope: **Pass**.
- Cross-tenant identical IDs remain isolated: **Pass**.
- Transaction ownership is explicit and rollback is atomic: **Pass**.
- Domain hydration rejects corrupt persistence: **Pass**.
- Conversation history is visibility-filtered and keyset paginated: **Pass**.

P2-05 is complete.

