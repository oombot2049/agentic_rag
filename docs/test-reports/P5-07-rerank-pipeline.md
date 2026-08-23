# P5-07 Rerank Pipeline Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/rerank`

| Gate | Command | Result |
|---|---|---|
| Golden Set and properties | `go test -cover ./internal/rerank` | Pass; 88.4% statement coverage |
| 100-candidate benchmark | `go test -run '^$' -bench BenchmarkPipeline100Candidates -benchmem -benchtime=100x ./internal/rerank` | Pass; ~161 µs/op, 32,664 B/op, 223 allocs/op on local Windows host |
| Full suite | `go test ./...` | Pass |
| Static analysis | `go vet ./...` and `staticcheck ./...` | Pass |
| Workflow/patch lint | `actionlint -no-color`; `git diff --check` | Pass |

Tests cover normalized duplicate removal before provider invocation, valid
adjacency merge and citation retention, stable score ties, timeout/wrong-count/
NaN fallback, exact recount after compression, dependency failures and budgets
from 1 through 50 tokens. Benchmark values are local observations, not SLOs.
