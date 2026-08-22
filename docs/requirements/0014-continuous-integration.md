# Requirement P1-08: Continuous integration quality gates

- Status: Implemented
- Priority: P0
- Owner: DevOps
- Last reviewed: 2026-08-23
- Depends on: [P1-01 Go foundation](0007-go-project-foundation.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P1-08

## 1. Objective

Every pull request and main push must prove formatting, static correctness, test
behavior, race safety, real dependency integration, reproducible build and known
vulnerability status. A green job means its command executed; cached test results
and optional Docker skips are forbidden in mandatory jobs.

## 2. Workflow topology

| Job | Runtime | Mandatory evidence |
|---|---|---|
| quality | Ubuntu 24.04; Go 1.26.x and 1.27.x | mod verify, gofmt, vet, uncached shuffled tests, build |
| static-analysis | Ubuntu 24.04; Go 1.26.x | Staticcheck 2026.1 (`v0.7.0`) and actionlint `v1.7.12` |
| race-integration | Ubuntu 24.04; Go 1.27.x; Docker required | Docker health, PostgreSQL/Valkey integration, race |
| vulnerability | Ubuntu 24.04; Go 1.27.x | govulncheck `v1.7.0 ./...` |
| gate-self-test | Ubuntu 24.04; Go 1.27.x | deliberate format and test faults return non-zero |

All jobs have timeouts, least-privilege `contents: read`, and concurrency cancels
obsolete runs on the same pull request/ref.

## 3. Cache and reproducibility

`setup-go` caching is disabled because it also includes Go's build cache. Only
the module download directory is cached with `actions/cache v5.0.5`, keyed by OS,
Go version and `go.sum`. `GOCACHE` and test results are never cached. Tests use
`-count=1`; quality tests also shuffle order. `go mod verify` precedes builds.

Builds use `CGO_ENABLED=0`, `-buildvcs=false`, `-trimpath` and explicit metadata
through the canonical Make target.

## 4. Docker enforcement

Local Testcontainers tests may skip without a provider. CI first runs
`docker info`, sets `AGENTICRAG_REQUIRE_DOCKER=1`, and tests then bypass the skip
helper. Provider/container faults therefore fail the job, closing the local
P1-06/P1-07 verification gap.

## 5. Gate self-test

`ci/verify-gates.sh` creates an isolated temporary module, injects an unformatted
file and a failing test, and asserts both tools return non-zero. Repository files
are untouched. A Go contract test inspects workflow content for required commands,
uncached tests, Docker enforcement, timeouts and absence of `continue-on-error`.

## 6. Dependency and supply-chain policy

Official actions use exact release tags (`checkout v6.0.2`, `setup-go v7.0.0`,
`cache v5.0.5`) and Dependabot reviews Go modules/actions weekly. Full commit-SHA
pinning is preferred; network restrictions prevented resolving tag SHAs locally,
so security maintenance must replace tags with verified immutable SHAs. Go tools
are installed at exact module versions, never `latest`; actionlint validates the
workflow schema and expressions.

Staticcheck 2026.1 is deliberately built/run with Go 1.26: its stable analyzer
cannot decode Go 1.27 export data. Go 1.27 remains mandatory for quality, race,
integration and release build jobs; using the 2026.2 release candidate solely to
claim a newer analyzer was rejected.

## 7. Acceptance

- [x] gofmt, vet, Staticcheck, tests, race and govulncheck are mandatory jobs.
- [x] Docker skips cannot produce green mandatory integration CI.
- [x] Only downloaded modules are cached; tests always bypass result cache.
- [x] Deliberate formatting and test failures verify gate behavior.
- [x] Workflow policy is repository-tested and dependency automation configured.
