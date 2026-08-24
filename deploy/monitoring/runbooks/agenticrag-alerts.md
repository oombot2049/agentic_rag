# AgenticRAG Production Alert Runbooks

These procedures preserve tenant isolation, idempotency, auditability and final
commit durability. Never fix an incident by disabling ACL checks, clearing Redis,
rewriting queue rows manually, relaxing Agent limits, or logging payloads. Record
incident ID, region, deployment SHA, first symptom and every mitigation.

The rules assume deployment wiring for application metrics, `/metrics`,
dependency exporters, and governed analytics. The repository's bare `serve`
composition currently exposes only `/healthz`; validate telemetry presence and
freshness before interpreting absence of an alert as service health.

## AgenticRAGAvailabilityFastBurn

1. Declare an incident and stop the active rollout.
2. Split 5xx rate by deployment, status class and trace-linked operation; check
   PostgreSQL, Redis, object, search and LLM alerts.
3. Shed ingestion and speculative Runs before interactive work. Roll back only
   to a verified schema-compatible release.
4. Resolve after both burn windows remain below threshold for 30 minutes; attach
   request/audit reconciliation without customer content.

## AgenticRAGAvailabilitySustainedBurn

Freeze feature rollout, assign the service owner, compare the 30m and 6h error
sources, then remove the dominant failure or dependency. Keep the freeze until
both windows recover and the incident owner calculates remaining monthly budget.

## AgenticRAGHTTPP99LatencyHigh

Stop rollout; compare request rate, saturation and dependency spans. Preserve
read/cancel/SSE capacity, shed background ingestion, and scale only after proving
the bottleneck is compute rather than a locked database or retry storm. Resolve
after p99 stays below two seconds for 30 minutes.

## AgenticRAGRunP99LatencyHigh

Enter constrained admission, inspect queue wait versus LLM/retrieval/tool spans,
and stop the slow component's retries. Do not extend the 120-second Run deadline.
Verify new Runs terminalize and stuck Runs are recovered through fenced leases.

## AgenticRAGQueueBacklogHigh

1. Enable constrained admission and shed knowledge ingestion/speculative Runs.
2. Check worker replicas, lease claim errors, PostgreSQL locks and provider
   circuit breakers; never edit or truncate `run_queue_messages`.
3. Scale workers only if downstream headroom exists.
4. Resolve below 3,000 for 15 minutes and reconcile READY/LEASED rows with Runs.

## AgenticRAGQueueWaitP95High

Compare durable backlog, active Runs, worker claim rate and dependency latency.
Open a reliability ticket with the limiting resource and capacity evidence. If
backlog crosses 3,000, follow the queue backlog incident procedure.

## AgenticRAGWorkerSaturation

Enable critical admission before the 4,000 cap, reserve workers for cancellation
and recovery, and stop ingestion. Determine whether saturation is traffic or
latency driven before scaling; verify lease fencing and active gauge convergence.

## AgenticRAGLLMHourlyCostHigh

Reconcile the Prometheus estimate with the durable billing ledger. Attribute the
change to traffic, tokens, model mix or retries; check for a retry loop. Apply a
tenant-approved lower-cost model or quota only through normal policy controls.

## AgenticRAGPostgresUnavailable

Reject mutations and new Runs; keep only proven-safe reads. Check quorum,
replication lag, storage and connection exhaustion. Use the managed failover
procedure—never promote an unverified replica. Before reopening writes, verify
migrations, final commits, leases, outbox and queue reconciliation.

## AgenticRAGRedisUnavailable

Preserve PostgreSQL as authority. Disable cache-only optimizations and constrain
new Runs if quota admission cannot fail closed. Check cluster quorum, memory and
network; do not flush keys. Verify `/events` replay from durable lifecycle events
and quota saga reconciliation after recovery. Treat `/output` gaps as degraded
presentation: clients must load the current Run and final Message rather than
expecting PostgreSQL to replay missing assistant deltas.

## AgenticRAGObjectStorageUnavailable

Stop Artifact-producing Steps and ingestion while preserving metadata and audit
rows. Check provider status, DNS, credentials and KMS separately. Run a synthetic
write/read/delete with a non-tenant probe object before resuming; never expose
object locators in the incident channel.

## AgenticRAGSearchIndexUnavailable

Stop index promotion and online retrieval admission. Check cluster health,
generation alias and ACL filter behavior. Use a fallback only when configured and
equally tenant-safe. Verify a synthetic query plus generation consistency before
resuming ingestion.

## AgenticRAGLLMProviderFailureHigh

Open the provider circuit and stop retry amplification. Compare failed,
timed-out and rate-limited outcomes by bounded provider/model labels. Switch only
to a tenant-approved compatible model, respecting token and cost budgets. Resolve
after success rate and TTFT recover for 15 minutes.
