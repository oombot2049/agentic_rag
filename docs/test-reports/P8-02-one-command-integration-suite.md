# P8-02 One-command Integration Suite Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `tests/integration`, Makefile and Linux CI

| Gate | Command | Result |
|---|---|---|
| Docker-free image/entrypoint contracts | `go test -v ./tests/integration` | Pass; pinned image and CI/Make contracts |
| Integration-tag compilation | `go test -tags=integration -run '^$' ./tests/integration` | Pass on Go 1.26 and 1.27 |
| Tagged vet/static analysis | `go vet -tags=integration ./tests/integration`; `staticcheck -tags integration ./tests/integration` | Pass |
| Real service suite | `go test -tags=integration -count=1 -v ./tests/integration` | Mandatory Linux Docker CI gate; local Windows rootless Docker unavailable |
| Go 1.26 default suite | `go test -count=1 -shuffle=on ./...` | Pass |
| Go 1.27 default suite | `go test -count=1 ./...` | Pass |
| Dependencies | `go mod tidy`; `go mod verify` | Pass |
| Formatting/patch hygiene | repository-scoped `gofmt`; `git diff --check` | Pass |
| Race detector | Ubuntu CI `go test -race -count=1 ./...` | Mandatory CI gate |

The explicit suite pins PostgreSQL `18.6-alpine`, Valkey `9.1.0-alpine`, MinIO
`RELEASE.2025-09-07T16-13-09Z` and Qdrant `v1.15.5`. Docker-free tests reject
floating tags and prove both `make integration` and CI execute the exact tagged
command.

PostgreSQL applies the full migration history and the production Queue adapter
proves tenant-local claim/ack. The production Valkey key builder/client and S3
SigV4 adapter prove tenant-key isolation. Qdrant stores equal vectors for two
tenant payloads and its query must return only the filtered tenant. Every
container collects at most 256 KiB of logs on failure before cleanup. The real
container executions cannot run on the local Windows rootless environment and
are therefore explicitly enforced by the existing Linux Docker/race job.
