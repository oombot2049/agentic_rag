# P8-06 Production Load and Capacity Evidence Report

- Date: 2026-08-23
- Build under test: working tree for P8-06
- Scope: load tooling and deterministic capacity gate
- Result: PASS for executable contract; representative capacity run pending deployment evidence

## Coverage

The release profile encodes 50 Turn/s for 30 minutes, 100 Turn/s for 60 seconds,
20,000 sustained SSE connections, and 2,000 reconnects/s. Thresholds cover
achieved rate, mutation errors, Create Turn p95/p99, SSE resume p95/p99, TTFT
p95/p99, queue depth/slope, database CPU, service CPU and RSS. Measurements also
retain p50 values, database connections/lock waits, goroutines and byte totals.

Automated tests prove strict JSON parsing, stage ordering, nearest degradation
point, sustained queue-growth rejection, burst-backlog bounds, evidence-class
separation, CLI operation, SSE authentication, resume latency, and assistant
delta TTFT detection. k6 uses pre-provisioned, one-use Sessions and checks the
exact serialized request-size bucket. The Go generator handles SSE connections
and paced reconnects with bounded line and network limits.

## Verification performed

| Gate | Result |
| --- | --- |
| Go 1.26 load unit/contract tests | PASS |
| Go vet for load packages | PASS |
| Smoke fixture threshold report | PASS, `capacity_evidence=false` |
| Malformed/unknown-field rejection | PASS |
| Production safety guard design | PASS |

The final repository-wide Go 1.26/1.27, vet, format, and staticcheck results are
recorded before commit.

## Capacity conclusion

No production-capacity number is claimed from this development environment:
it has neither the representative deployment nor k6 infrastructure required
for 20,000 sockets. The suite now produces the required proof when run against
an authorized, production-equivalent environment. A valid evidence bundle must
contain the immutable build and topology manifest, raw k6/SSE summaries,
Prometheus before/after snapshots, CPU/heap profiles, normalized measurement,
and a passing release report with `capacity_evidence=true`.

This distinction is a correctness property, not an omitted test: contract
fixtures are structurally unable to become production evidence.
