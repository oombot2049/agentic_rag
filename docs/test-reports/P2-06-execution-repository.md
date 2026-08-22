# P2-06 execution repository test report

- Date: 2026-08-23
- Commit under test: working tree before P2-06 commit
- Toolchains: Go 1.27.0 and Go 1.26.7, windows/amd64
- Database target: PostgreSQL 18.6 in mandatory Linux CI
- Requirement: [0020 execution repository](../requirements/0020-execution-repository.md)

## 1. Result

P2-06 passes lease classification, fencing predicates, Run/Step/Attempt
hydration, checkpoint monotonicity, migration, full-repository and static-analysis
gates. The expanded `internal/repository` package has 75.9% statement coverage.
A real PostgreSQL 40-worker lease race and takeover test is checked in; local
Docker absence is explicit and mandatory mode fails rather than skips.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./internal/repository ./internal/platform/postgres ./migrations` | Pass |
| Repository coverage | `go test -cover ./internal/repository` | Pass; 75.9% across conversation + execution adapters |
| Randomized repetition | `go test -shuffle=on -count=100 ./internal/repository ./migrations` | Pass |
| PostgreSQL package | `go test ./internal/platform/postgres` | Pass; Docker tests explicitly skipped locally |
| Mandatory provider behavior | `AGENTICRAG_REQUIRE_DOCKER=1 go test -run '^TestExecutionRepositoryLeaseTakeoverStepAttemptAndCheckpoint$' ./internal/platform/postgres` | Expected non-zero without Docker |
| Full Go 1.27 regression | `go test -count=1 ./...` | Pass |
| Go 1.27 vet/build | `go vet ./...`; `go build -buildvcs=false -trimpath ...` | Pass |
| Full Go 1.26 regression | `GOTOOLCHAIN=local go test -count=1 ./...` | Pass |
| Go 1.26 vet | `GOTOOLCHAIN=local go vet ./...` | Pass |
| Staticcheck | `staticcheck ./...` with Go 1.26.7 | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Lease and SQL evidence

Scripted `database/sql` tests execute the repository SQL paths and verify:

- acquire returns incremented execution version/token and hydrated lease;
- stale expected version returns row conflict; a live owner returns lease held;
- renew preserves token and increments version; release clears owner/expiry;
- wrong owner/token is fenced before stale-version classification;
- matching but expired authority returns lease expired;
- invalid owner, ID, token, version, time and lease duration fail before SQL;
- Run insertion and Run/Step/Attempt reads preserve tenant scope and domain
  validation; Step hydration restores ordered Attempt IDs;
- Step/Attempt writes with zero affected rows return fenced/conflict outcomes;
- invalid/oversized checkpoint payloads fail, successful payloads are copied,
  and a backward step number returns a dedicated checkpoint conflict.

Every child mutation SQL contains tenant, Run ID, owner, lease token, expected
execution version and an unexpired-at-command-time predicate. Step and Attempt
transitions additionally match their own row version and legal source state.

## 4. PostgreSQL concurrency specification

The checked-in integration test races 40 workers at Run version one. Exactly one
acquires token one/version two. It renews to version three, then a second worker
takes over at exact expiry and receives token two/version four. The old worker's
renew is rejected as fenced.

Under the takeover authority the test inserts and advances a Step, appends and
finishes an Attempt, rejects a stale Step version, saves/loads a JSON checkpoint
and rejects backward checkpoint movement. Release advances Run version to five;
the old authority can no longer save a checkpoint. A foreign tenant cannot load
the Run.

The test uses caller-supplied command time for both state timestamps and lease
expiry predicates, including the corrected Step/Attempt rule that prevents an
old authority timestamp from extending write access.

## 5. Migration evidence

Migration 000003 adds positive Run execution version, monotonic lease token,
bounded lease owner/expiry shape, Step/Attempt row versions, tenant-scoped
checkpoint storage with 1 MiB JSON bound, and a tenant-leading lease-scan index.
Static tests verify the contract and paired Down file. The complete migration
round trip now rolls all three versions down and reapplies them.

## 6. Acceptance conclusion

- Lease acquire/renew/release/takeover use CAS and monotonic tokens: **Pass**.
- Old workers cannot write after takeover: **Pass**.
- Step and Attempt state writes are row-version and lease fenced: **Pass**.
- Checkpoints are tenant-scoped, bounded and monotonic: **Pass**.
- Run/Step/Attempt persistence hydrates validated domain values: **Pass**.

P2-06 is complete.

