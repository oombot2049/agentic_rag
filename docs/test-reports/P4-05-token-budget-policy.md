# P4-05 Model-Exact Token Budget — Test Report

- Date: 2026-08-23
- Scope: model tokenizer selection, output reservation, prompt admission, truncation and explainability
- Result: PASS for all executable local gates; race execution remains mandatory in Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Window safety | The Budgeter subtracts output reservation and safety margin before admission, charges per-entry framing overhead, recounts truncated content and asserts the final input limit. |
| Exact tokenizer | The Registry maps an exact model ID to its declared window and tokenizer; an unknown model has no heuristic fallback and fails closed. |
| Mandatory context | System entries are never truncated or dropped. Current-user input is retained and may only be tokenizer-truncated with an explicit decision. |
| Stable priority | Complete history pairs are evaluated newest first, followed by memory and then ranked RAG evidence; returned entries retain canonical prompt order. |
| Structural safety | History is retained only as a complete user/assistant pair. Duplicate or malformed RAG evidence and non-compliant tokenizer output are rejected. |
| Explainability | Every original entry produces a stable kept, dropped or truncated decision with source, token counts and reason code. |
| Immutability | Mutating request evidence or slices returned by Plan accessors cannot alter the completed Plan. |
| Property coverage | 1,000 deterministic window/reservation combinations prove every successful Plan remains inside the model window and contains no partial history pair. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint -no-color                                        PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/contextmgr statement coverage                      86.1%
Budgeter deterministic repetition (100x)                    PASS
Budgeter window property cases per execution                1,000
```

The local Windows host cannot execute `go test -race` because the installed Go
toolchains have no CGO compiler. This is not reported as a local pass;
`.github/workflows/ci.yml` requires `go test -race -count=1 ./...` on Ubuntu.
