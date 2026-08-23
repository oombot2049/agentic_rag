# P4-10 Final Commit Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/turn`, `internal/repository`

## Evidence

| Gate | Command | Result |
|---|---|---|
| Unit and repository tests | `go test ./internal/turn ./internal/repository` | Pass |
| Package coverage | `go test -cover ./internal/turn ./internal/repository` | Pass; 85.9% / 45.6% (repository includes broad integration adapters) |
| Full suite | `go test ./...` | Pass |
| Static analysis | `go vet ./...` and `staticcheck ./...` | Pass |
| Patch hygiene | `git diff --check` | Pass |
| PostgreSQL concurrency | `TestFinalCommitConcurrentReplayAndStaleWorkerFencing` | Checked in; skipped locally when Docker provider is unavailable |
| PostgreSQL rollback | `TestFinalCommitFailureRollsBackEveryParticipant` | Checked in; skipped locally when Docker provider is unavailable |

The deterministic tests validate input boundaries, store error preservation,
new-Turn/regeneration rules, lease owner/token/execution-version fencing and
lease expiry. The Testcontainers suite verifies the actual schema and transaction
adapter in CI, where Docker is mandatory. Local verification does not claim a
Docker-backed pass.
