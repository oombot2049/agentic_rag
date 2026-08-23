# P7-03 Hierarchical Tenant Quota Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/quota`, Redis/Valkey atomic quota adapter

| Gate | Command | Result |
|---|---|---|
| Quota unit and concurrency tests | `go test -cover ./internal/quota` | Pass; 92.2% statement coverage |
| Repeatability | `go test -count=10 ./internal/quota` | Pass |
| Go 1.26 suite | `go test ./...`; `go vet ./...` | Pass |
| Go 1.27 suite | `go test ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...` | Pass |
| Dependencies | `go mod tidy`; `go mod verify` | Pass |
| Formatting/patch hygiene | repository-scoped `gofmt -l`; `git diff --check` | Pass |
| Real Valkey concurrency | `go test -run TestQuotaStoreIntegration ./internal/platform/redis` | Test present; local Windows skipped because rootless Docker is unavailable; mandatory Linux CI gate |
| Race detector | Ubuntu CI `go test -race -count=1 ./...` | Mandatory CI gate; local Windows environment has no CGO race runtime |

The deterministic suite verifies global-first admission, tenant denial with
global compensation, tenant isolation, cross-tenant reservation-ID isolation,
QPS refill, period rollover, Active Run lease expiry, Token/cost denial,
settlement refunds, compensation and outcome-ambiguous store failures. Two
100-goroutine hotspot runs admit exactly seven Runs for each tenant.

The Testcontainers suite targets Valkey 9.1 with 200 simultaneous reservations.
It asserts exact Active Run and QPS caps, independent tenant slots, post-settle
reuse, idempotent receipt state transitions and exact counters near `2^53-1`.
CI sets `AGENTICRAG_REQUIRE_DOCKER=1`, turning provider absence into a failure.
