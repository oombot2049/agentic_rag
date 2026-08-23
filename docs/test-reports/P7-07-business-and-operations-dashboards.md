# P7-07 Business and Operations Dashboards Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `deploy/dashboards`

| Gate | Command | Result |
|---|---|---|
| Dashboard/provisioning tests | `go test -v ./deploy/dashboards` | Pass; 4 contract/data tests |
| Repeatability | `go test -count=10 ./deploy/dashboards` | Pass |
| Go 1.26 full suite | `go test -count=1 -shuffle=on ./...` | Pass |
| Go 1.27 full suite | `go test -count=1 ./...` | Pass |
| Vet and static analysis | `go vet ./...`; `staticcheck ./...` | Pass |
| Dependencies | `go mod tidy`; `go mod verify` | Pass |
| Formatting/patch hygiene | repository-scoped `gofmt`; `git diff --check` | Pass |
| Race detector | Ubuntu CI `go test -race -count=1 ./...` | Mandatory CI gate |

The operations dashboard has thirteen panels covering availability/active pages,
HTTP volume and latency, queue backlog/wait, active Runs, terminal outcomes and
latency, retrieval SLI, LLM outcome/TTFT/cost and dependency health. Tests reject
tenant, Session, Turn, Run, request and object identities anywhere in its JSON.

The business dashboard has seven governed analytics panels covering the
distinct-Run funnel, stage conversion, committed Runs, success rate, durable
ledger cost, projection lag and source/projection gaps. Every SQL target joins
the viewer-RLS `dashboard_tenant_scope` and uses `${tenant:sqlstring}`. A fixture
with duplicate retrieval events proves one Run is counted once per stage and
that funnel counts remain monotonic. Provisioning is immutable, Git-owned, UTC
and refresh-bounded.
