# P2-02 core PostgreSQL schema test report

- Date: 2026-08-23
- Commit under test: working tree before P2-02 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Database target: PostgreSQL 18.6 in mandatory Linux CI
- Requirement: [0016 core PostgreSQL schema](../requirements/0016-core-postgresql-schema.md)

## 1. Result

P2-02 passes embedded-migration, static schema-contract, migration-loader,
full-repository, vet, Staticcheck and build gates. A real PostgreSQL 18.6
Testcontainers suite is checked in for apply/idempotence/constraint/down/up
verification. Docker is unavailable on this Windows host, so that suite reports
an explicit local skip; mandatory mode was separately exercised and correctly
failed instead of silently skipping.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./migrations ./internal/platform/postgres` | Pass |
| Schema contract | `go test ./migrations` | Pass |
| Randomized schema tests | `go test -shuffle=on -count=100 ./migrations` | Pass |
| PostgreSQL package | `go test -count=1 ./internal/platform/postgres` | Pass; integration tests explicitly skipped without Docker |
| Mandatory provider behavior | `AGENTICRAG_REQUIRE_DOCKER=1 go test -run '^TestCoreSchemaMigrationRoundTripAndConstraints$' ./internal/platform/postgres` | Expected non-zero; unavailable provider is a failure, not a skip |
| Full Go 1.27 regression | `go test -count=1 ./...` | Pass |
| Go 1.27 vet/build | `go vet ./...`; `go build -buildvcs=false -trimpath ...` | Pass |
| Full Go 1.26 regression | `GOTOOLCHAIN=local go test -count=1 ./...` | Pass |
| Go 1.26 vet | `GOTOOLCHAIN=local go vet ./...` | Pass |
| Staticcheck | `staticcheck ./...` with Go 1.26.7 | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Static schema evidence

| Contract | Evidence |
|---|---|
| Exact core table set | Parser finds only Session, Turn, Message, Run, Step and Attempt tables |
| Tenant isolation | Every table has non-null tenant column and tenant-first composite primary key |
| Parent ownership | Required composite Session/Turn/Run foreign-key fragments are present |
| Deferred cycles | Six final-commit foreign keys are declared `DEFERRABLE INITIALLY DEFERRED` |
| Ordered uniqueness | Turn sequence, Run number, Step number and Attempt number constraints present |
| State vocabulary | Frozen statuses including cancel-requested, superseded, timed-out and unknown present |
| Query indexes | Conversation history, active Runs, one user Message and unknown Attempt indexes present |
| Index isolation | Every explicit business index begins with `tenant_id` |
| Time semantics | At least ten instant columns found; every one is `timestamptz`; forbidden timestamp type absent |
| No Branch | Case-insensitive schema scan finds no Branch table/column/reference |
| Reversibility | Down file drops all six tables in reverse dependency order and preserves the shared ledger |

## 4. Real PostgreSQL suite specification

The checked-in integration test performs the following against PostgreSQL 18.6:

1. apply migration and repeat Up to verify idempotence;
2. introspect all six tables, mandatory tenant columns, timestamp types and six
   deferred aggregate foreign keys;
3. insert Session + Turn + user Message + initial Run in one transaction;
4. insert Step/Attempt and atomically select an assistant Message in a final
   transaction spanning Run, Turn, Message and Session;
5. store the same opaque IDs for two tenants, proving composite identity;
6. reject a Turn whose tenant has no matching Session;
7. reject a duplicate Attempt number under the same Step;
8. execute Down and verify all six tables are absent;
9. execute Up again and re-introspect the recreated schema.

P1-08 runs integration tests with `AGENTICRAG_REQUIRE_DOCKER=1` and `-race` on a
Linux Docker runner. This test report deliberately distinguishes the checked-in
real-database evidence from what could be executed on the current Docker-less
host.

## 5. Acceptance conclusion

- All business tables carry mandatory tenant scope: **Pass**.
- Unique constraints and tenant-leading query indexes are complete: **Pass**.
- Parent and tenant mixing is rejected by composite foreign keys: **Pass**.
- Every timestamp is timezone-aware: **Pass**.
- Transactional up/down migration and real PostgreSQL round-trip suite exist: **Pass**.
- Local mandatory-provider behavior cannot degrade to a skip: **Pass**.

P2-02 is complete.

