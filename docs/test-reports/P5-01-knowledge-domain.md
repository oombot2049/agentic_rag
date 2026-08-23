# P5-01 Knowledge Domain Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/knowledge`

| Gate | Command | Result |
|---|---|---|
| Domain tests | `go test -cover ./internal/knowledge` | Pass; 92.4% statement coverage |
| Full suite | `go test ./...` | Pass |
| Static analysis | `go vet ./...` and `staticcheck ./...` | Pass |
| Workflow lint | `actionlint -no-color` | Pass |
| Patch hygiene | `git diff --check` | Pass |

The suite exhaustively checks all Version transition pairs, KnowledgeBase
terminal lifecycle, ACL canonicalization/default deny/permission implication,
defensive copying, duplicate Chunk ordinal/ID, byte-exact content hashing and
1,000 randomized cross-tenant or mismatched-parent attempts. Forged same-tenant
ACLs and future ACL revisions are also rejected.
