# P1-04 application error model test report

- Date: 2026-08-23
- Commit under test: working tree before P1-04 commit
- Toolchain: Go 1.27.0 windows/amd64
- Requirement: [0010 application errors](../requirements/0010-application-errors.md)

## 1. Result

P1-04 passes its complete registry, HTTP mapping, error-chain, non-disclosure,
static-analysis and build gates. `internal/apperror` has 97.4% statement
coverage.

## 2. Automated evidence

| Gate | Command | Result |
|---|---|---|
| Format | `go fmt ./...` | Pass |
| Unit + coverage | `go test -cover ./internal/apperror` | Pass; 97.4% |
| Randomized repetition | `go test -shuffle=on -count=50 ./internal/apperror` | Pass |
| Full regression | `go test ./...` | Pass |
| Static analysis | `go vet ./...` | Pass |
| Build | `go build -buildvcs=false -trimpath -o bin/agenticrag.exe ./cmd/agenticrag` | Pass |
| Patch hygiene | `git diff --check` | Pass |

## 3. Contract matrix

| Scenario | Expected | Evidence |
|---|---|---|
| All 23 registered codes | Unique uppercase code, non-empty safe message, exact HTTP status | Registry table test passes |
| Same HTTP status, different occurrence | Retryability can differ | Occurrence test passes |
| Wrapped repository error | `errors.Is` reaches root cause | Chain test passes |
| Wrapped application error | `errors.Is` matches same code and `errors.As` returns typed error | Chain test passes |
| Unrelated/invalid code | Does not match | Negative matching tests pass |
| Unknown Go error | Normalizes to non-retryable `INTERNAL` | Normalization test passes |
| Invalid constructed code | Falls back to `INTERNAL`, preserves private cause | Defensive test passes |
| Nil input/receiver | Safe 500/`INTERNAL` behavior without panic | Defensive test passes |
| Secret/SQL/provider marker in cause | Absent from `Error`, public message and JSON | Leak test passes |
| Returned code list modified | Registry remains unchanged | Copy test passes |

## 4. Safety notes

The leak test places a password, SQL fragment and private table marker in the
wrapped cause. The cause remains discoverable through `errors.Is`, while none of
those markers appears in the error string or JSON public view. Public messages
come exclusively from the closed registry; constructors accept no arbitrary
public text.

## 5. Race detector limitation

This package is immutable and has no shared mutable state after initialization.
The Windows host still lacks the C compiler required for `-race`; P1-08 will run
the repository race gate on Linux.

## 6. Acceptance conclusion

- Stable codes and centralized HTTP mapping: **Pass**.
- Internal errors do not leak to public representations: **Pass**.
- `errors.Is/As` and cause unwrapping: **Pass**.
- Complete table-driven registry coverage and quality gates: **Pass**.

P1-04 is complete.
