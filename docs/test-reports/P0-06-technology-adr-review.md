# P0-06 validation report: technology selection

- Result: Passed
- Date: 2026-08-23
- Validation type: ADR completeness and boundary review

## Selection coverage

| Capability | Reference | Alternatives documented | Exit documented |
|---|---|---|---|
| Go | 1.27 toolchain, 1.26 language baseline | Pass | Pass |
| PostgreSQL | 18 reference, 16–18 support | Pass | Pass |
| Redis-compatible transient state | Valkey 9.1 reference | Pass | Pass |
| Queue | PostgreSQL lease queue + outbox | Pass | Pass |
| Object storage | S3 API | Pass | Pass |
| Vector/hybrid search | OpenSearch 3.x / 3.8 reference | Pass | Pass |
| Knowledge MCP | Retriever adapter | Pass | Pass |

## Boundary checks

- Domain packages are prohibited from importing provider SDKs.
- Application interfaces use domain/primitive types and are consumer-owned.
- PostgreSQL is the only final source of truth.
- Cache and queue state can be rebuilt or migrated without changing resource IDs.
- Network calls are prohibited inside database transactions.
- Queue replacement retains outbox delivery ID, fencing and consumer idempotency.
- Search replacement uses immutable-version rebuild, shadow queries, ACL/relevance
  comparison and alias/adapter cutover.

## Exit-threshold checks

- Queue thresholds include throughput, database budget, lease latency and
  independent scaling/recovery.
- Search thresholds include p95 latency, relevance, chunk scale, cost and data
  residency/availability.
- PostgreSQL replacement is correctly described as a behavioral migration, not
  an effortless repository swap.

## Source review

Official project sources were recorded for Go releases/support, PostgreSQL
versions and locking, Valkey versions/support, OpenSearch versions/hybrid search
and the S3 API. Exact deployed patches/digests remain automation-owned.

## Conclusion

P0-06 provides a concrete testable stack and explicit alternatives, constraints
and exit strategies while keeping queue, search/vector and MCP dependencies
behind stable Go ports.
