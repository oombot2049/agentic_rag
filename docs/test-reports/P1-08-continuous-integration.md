# P1-08 continuous integration test report

- Date: 2026-08-23
- Local toolchains: Go 1.26.7 and Go 1.27.0 windows/amd64
- Requirement: [0014 continuous integration](../requirements/0014-continuous-integration.md)

## Result

Workflow contracts, YAML/action expressions, two-Go-family tests, formatting,
vet, Staticcheck, vulnerability scanning, build and deliberate-failure gates
pass locally. Linux race and successful real-container execution cannot run on
this Windows host; the workflow converts both into mandatory Docker/Linux jobs.
The first GitHub-hosted run remains external evidence to collect after push.

## Executed evidence

| Gate | Result |
|---|---|
| actionlint v1.7.12 | Pass; `.github/workflows/ci.yml` has no findings |
| CI Go contract tests | Pass; required commands/versions/cache policy present |
| `ci/check-format.sh` | Pass in Git Bash |
| `ci/verify-gates.sh` | Pass; deliberate gofmt and test faults both rejected |
| Go 1.26.7 `go test -p=1 -count=1 ./...` | Pass, all packages |
| Go 1.26.7 `go vet ./...` | Pass |
| Staticcheck 2026.1 / v0.7.0 under Go 1.26.7 | Pass |
| Go 1.27.0 `go test -p=1 -count=1 ./...` | Pass, all packages |
| Go 1.27.0 `go vet ./...` | Pass |
| Go 1.27.0 reproducible/trimmed build | Pass |
| `git diff --check` | Pass |
| govulncheck v1.7.0 | Pass after remediation: `No vulnerabilities found` |

Go 1.26.7 was downloaded from go.dev and verified against official SHA-256
`f4f534a486e4bc3387fa18f08208f2f854b7aaea8a08f2a2d829a914a05abb11`.
The portable toolchain and tools are ignored by Git.

## Findings resolved during validation

### Reachable dependency vulnerability

The first govulncheck run found `GO-2026-5970`, an infinite loop on invalid input
in `golang.org/x/text v0.37.0`, with reachable traces through the PostgreSQL
driver. The module was upgraded to fixed `v0.39.0` (and compatible x/sync
v0.21.0), all tests were rerun, and the second scan found no vulnerabilities.

### Staticcheck/compiler compatibility

Staticcheck stable v0.7.0 cannot decode Go 1.27 export data. The static-analysis
job was correctly moved to Go 1.26.x instead of adopting a release candidate.
Go 1.27 remains covered by quality, integration/race and build jobs. Staticcheck
then identified three intentional nil-context contract tests; each has a narrow,
reasoned suppression, and no production call suppresses the rule.

### Docker skip enforcement

With `AGENTICRAG_REQUIRE_DOCKER=1`, both PostgreSQL and Valkey integration tests
were deliberately run on the no-Docker host. Both failed with exit code 1 rather
than skipping, proving the mandatory CI mode fails closed. In normal local mode
they continue to report an explicit skip.

## Cache and failure review

- setup-go cache is disabled;
- only `GOMODCACHE` is cached, keyed by OS/Go/go.sum;
- every test command uses `-count=1`; no Go build/test cache is persisted;
- no `continue-on-error` or `latest` tool selector exists;
- all jobs have timeouts and read-only repository permission;
- Dependabot covers Go modules and Actions weekly.

## Remaining external evidence

The workflow has not yet run on GitHub because no remote publication was
requested/configured. On first push, required evidence is: both quality matrix
rows green, Staticcheck/actionlint green, Docker-backed integration plus race
green, govulncheck green and gate-self-test green. A failure must block merge.

P1-08 implementation and local verification are complete.
