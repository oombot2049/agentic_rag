# P4-07 Provider-Neutral LLM Gateway — Test Report

- Date: 2026-08-23
- Scope: Provider abstraction, response validation, deadlines, retry, circuit breaker and Token/cost accounting
- Result: PASS for all executable local gates; race execution remains mandatory in Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Provider isolation | Gateway inputs, outputs and errors are package-owned canonical types; the Fake HTTP adapter translates at its private boundary. |
| Deadline | Every physical call observes an attempt deadline; deadline expiry becomes typed TIMEOUT and caller cancellation interrupts backoff. |
| Retry classification | RATE_LIMITED, TIMEOUT and UNAVAILABLE retry; invalid request, authentication, safety, bad response, adapter error and untyped errors do not. |
| Stable side-effect key | HTTP integration observes the same Idempotency-Key across a 503 retry and successful second attempt. |
| Bounded retry | Tests prove retry-after capping, exponential/full-jitter boundary and exact max-attempt accounting. |
| Circuit breaker | Two failures open the configured breaker; 32 concurrent contenders are rejected while exactly one half-open probe reaches Provider. |
| Recovery/re-open | Successful probe closes/reset the breaker; failed probe reopens it and an abandoned caller probe permits a later health probe. |
| Structured output | JSON shape, text/structured exclusivity and injected schema validator must pass before success; safety finish is a typed rejection. |
| Usage and price | Token totals are checked, exact route pricing uses rounded checked integer math, and overflow fails closed. |
| Immutability | Provider and validator mutate their input copies without changing caller Request or returned Result; one immutable scalar record is emitted. |
| HTTP integration | `httptest` fake upstream returns 503 then a normalized response; Gateway retries once, validates usage and calculates exact cost. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint -no-color                                        PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/llm statement coverage                             79.6%
Fake Provider / breaker / HTTP suite repetition (100x)      PASS
```

The local Windows host cannot execute `go test -race ./internal/llm` because
the installed Go toolchains have no CGO compiler. This is not reported as a
local pass; `.github/workflows/ci.yml` requires `go test -race -count=1 ./...`
on Ubuntu.
