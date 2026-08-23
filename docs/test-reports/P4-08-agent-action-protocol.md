# P4-08 Strict Agent Action Protocol — Test Report

- Date: 2026-08-23
- Scope: SEARCH/TOOL/FINAL schema, semantic validation, repair and chain-of-thought exclusion
- Result: PASS for all executable local gates; race execution remains mandatory in Linux CI

## Acceptance evidence

| Requirement | Evidence |
|---|---|
| Discriminated union | All three variants decode, expose exactly one typed payload and round-trip through canonical validated encoding. |
| Strict JSON | Unknown fields, case-insensitive aliases, duplicate keys at any nesting level, trailing values, missing required fields and multiple payloads fail. |
| SEARCH contract | Query/top-k/filter sizes, explicit unique knowledge-base IDs and JSON-object filters are enforced. |
| TOOL contract | Canonical dotted names and bounded JSON-object arguments are required. |
| FINAL contract | Nonblank bounded answer plus valid unique Artifact citation IDs are required. |
| No hidden CoT | `thought`, `chain_of_thought`, free-form `reason` and every unknown reasoning field are rejected; only uppercase `reason_code` survives. |
| No disclosure | Parse errors expose stable failure codes only; a secret in rejected output never appears in error text or Result. |
| Repair | Invalid output is passed by defensive copy to at most one Repairer call and replacement bytes run through the identical strict decoder. |
| Immutability | Schema, filters, arguments, knowledge-base IDs and citations are returned as defensive copies. |
| Gateway integration | Validator statically satisfies `llm.StructuredValidator`, accepts only the canonical Action schema and runs semantic Decode. |
| Fuzz | Native Go fuzz executed 728,608 cases with no panic, ambiguous Action or non-round-trippable success. |

## Gates executed

```text
Go 1.26: go test ./...                                      PASS
Go 1.26: go vet ./...                                       PASS
Go 1.26: staticcheck ./...                                  PASS
actionlint -no-color                                        PASS
Go 1.27: go test ./...                                      PASS
Go 1.27: go vet ./...                                       PASS
internal/agent/action statement coverage                    88.2%
contract/boundary deterministic repetition (100x)           PASS
native Go fuzz (5 seconds)                                  PASS, 728,608 executions
```

The local Windows host cannot execute `go test -race ./internal/agent/action`
because the installed Go toolchains have no CGO compiler. This is not reported
as a local pass; `.github/workflows/ci.yml` requires
`go test -race -count=1 ./...` on Ubuntu.
