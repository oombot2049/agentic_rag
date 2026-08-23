# P4-09 Durable Agent Execution Budget — Test Report

- Date: 2026-08-23
- Scope: Step/time/Token/Tool/cost limits, reservation settlement, checkpoint restore and Action loops
- Result: PASS for all executable local gates; race execution remains mandatory in Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Hard pre-call limit | Model input + maximum output Token and worst-case cost are reserved before invocation; Step and Tool counters are admitted before execution. |
| Exact boundary | Exact Token/cost/Step/Tool capacity succeeds; a one-unit projection returns unchanged Ledger plus COMPLETED_WITH_LIMIT. |
| Deadline | Equality with the absolute Run deadline returns MAX_DURATION before reservation/admission; Deadline is exposed for external-call context binding. |
| Idempotent takeover | Repeating the exact operation-key reservation returns an identical Ledger; a different operation is rejected while outstanding. |
| Settlement | Provider input/output/cost must individually fit the reservation; mismatch cannot mutate committed totals. |
| Gateway bound | LLM Gateway now rejects provider output usage above the requested maximum output tokens before budget settlement. |
| Consecutive loop | The third identical Action is rejected when the configured maximum is two, even when each `reason_code` differs. |
| Cycle loop | Alternating `A,B,A,B,A,B` is detected before the sixth Action is executed. |
| Durable restore | JSON round-trip preserves Ledger and outstanding reservation; caller mutation cannot alter restored state. |
| Corruption defense | Unknown schema, negative/over-limit counters, malformed digests and over-budget reservations fail closed. |
| Property invariant | 1,000 deterministic randomized Runs with up to 50 operations never exceed any limit and rejected operations retain prior state. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint -no-color                                        PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/agent/budget statement coverage                    83.7%
budget boundary/restore/loop repetition (100x)              PASS
randomized budget property Runs                             1,000
```

The local Windows host cannot execute
`go test -race ./internal/agent/budget ./internal/llm` because the installed Go
toolchains have no CGO compiler. This is not reported as a local pass;
`.github/workflows/ci.yml` requires `go test -race -count=1 ./...` on Ubuntu.
