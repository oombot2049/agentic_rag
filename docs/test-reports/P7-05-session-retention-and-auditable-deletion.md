# P7-05 Session Retention and Auditable Deletion Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/compliance`, migration `000015`

| Gate | Command | Result |
|---|---|---|
| State machine and migration tests | `go test ./internal/compliance ./migrations` | Pass |
| Hermetic package coverage | `go test -cover ./internal/compliance` | Pass; 42.6% including the Docker-backed PostgreSQL adapter |
| PostgreSQL end-to-end | `AGENTICRAG_REQUIRE_DOCKER=1 go test -v ./internal/compliance` | Mandatory Linux race-CI scenario; local Windows rootless Docker unavailable |
| Go 1.26 full suite | `go test -count=1 ./...` | Pass |
| Go 1.27 full suite | `go test -count=1 ./...` | Pass |
| Vet and static analysis | `go vet ./...`; `staticcheck ./...` | Pass |
| Dependencies | `go mod tidy`; `go mod verify` | Pass |
| Formatting/patch hygiene | repository-scoped `gofmt`; `git diff --check` | Pass |
| Race detector | Ubuntu CI `go test -race -count=1 ./...` | Mandatory CI gate |

The deterministic state-machine suite injects an external provider failure and
proves that Object, Redis and owned Index targets are idempotently retried before
the PostgreSQL cascade. It covers exact request replay, changed-payload conflict,
tenant isolation, legal-hold exclusion, lease takeover fencing, stable evidence
pagination, completed replay and retention that refuses to erase unfinished-job
evidence.

The Testcontainers scenario applies the complete migration history, creates a
Session graph and Artifact, freezes all three target kinds into the manifest,
executes deletion, then proves Session descendants, target registry and Artifact
metadata are absent while the independent job and four evidence transitions
remain. Finally, it purges those transitions in bounded batches. PostgreSQL
advisory locking serializes hold/request creation, row locks plus lease tokens
fence workers, and no provider error or locator is copied into evidence.
