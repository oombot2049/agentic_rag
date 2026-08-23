# P5-06 Hybrid Retriever Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/retrieval` hybrid retrieval

| Gate | Command | Result |
|---|---|---|
| Integration and authorization tests | `go test -cover ./internal/retrieval` | Pass; 89.0% combined statement coverage |
| Full suite | `go test ./...` | Pass |
| Static analysis | `go vet ./...` and `staticcheck ./...` | Pass |
| Workflow lint | `actionlint -no-color` | Pass |
| Patch hygiene | `git diff --check` | Pass |

Tests assert mandatory filters reach both routes before Top-K, scope and ACL
violations discard the entire route, both violations fail retrieval, and one
20 ms route timeout degrades to the successful route. Parent cancellation,
caller input immutability, overlapping Chunk fusion, rank explanation and
deterministic order are also covered.
