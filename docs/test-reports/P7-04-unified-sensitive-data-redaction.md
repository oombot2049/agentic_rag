# P7-04 Unified Sensitive Data Redaction Test Report

- Date: 2026-08-23
- Result: Pass
- Scope: `internal/redaction`, `internal/observability/log`, public error boundary

| Gate | Command | Result |
|---|---|---|
| Redaction unit/security tests | `go test -cover ./internal/redaction` | Pass; 92.5% statement coverage |
| Fuzz test | `go test -run '^$' -fuzz FuzzRedactNeverLeaksConstructedBearer -fuzztime=5s -parallel=2 ./internal/redaction` | Pass; 28,472 executions in the final run |
| Repeatability/concurrency | `go test -count=10 ./internal/redaction ./internal/observability/log` | Pass |
| Go 1.26 suite | `go test ./...`; `go vet ./...` | Pass |
| Go 1.27 suite | `go test ./...`; `go vet ./...` | Pass |
| Static analysis | `staticcheck ./...` | Pass |
| Dependencies | `go mod tidy`; `go mod verify` | Pass |
| Formatting/patch hygiene | repository-scoped `gofmt -l`; `git diff --check` | Pass |
| Race detector | Ubuntu CI `go test -race -count=1 ./...` | Mandatory CI gate; local Windows environment has no CGO race runtime |

Security cases cover sensitive field names and suffixes, credentials hidden in
ordinary values/messages/keys, error and arbitrary object values, nested and
malformed JSON, PII, custom RE2 rules, invalid policies, oversized/deep inputs,
resolver outage, and public application-error cause omission. Output expansion
and structure are bounded and fail closed.

A 200-goroutine isolation test shares immutable policies and proves a custom
tenant detector never affects the baseline tenant. Logger integration verifies
the authenticated correlation tenant selects the policy and the resolver is
called exactly once per complete record. Artifact admission rejects weak
encryption, excess TTL, unknown classification and policy outages.
