# Capacity and workload baseline

- Status: Accepted
- Priority: P0
- Owner: Architecture
- Last reviewed: 2026-08-25

## 1. Purpose and interpretation

This document defines the planning workload for the MVP. Values are sizing
assumptions, not customer promises or hard-coded limits. The SLO document derives
objectives from this workload; load tests later prove or revise the assumptions.

The tables distinguish implemented `AGENTICRAG_*` controls from planning keys.
Only the former are accepted by the current typed configuration module; dotted
keys are target control-plane names and must not be presented as deployable
environment variables. Production may change a planning envelope only after
the corresponding admission control and representative load evidence exist.

## 2. Capacity model

The sizing model uses four distinct terms:

- **Average**: 24-hour mean used for cost and storage projections.
- **Sustained peak**: load a deployment must serve for 30 minutes without
  intentional shedding.
- **Burst**: short spike admitted for up to 60 seconds while bounded queues absorb
  work.
- **Hard cap**: safety fence. Work above it is rejected or deferred predictably.

Never use average latency or average request rate to size worker concurrency.
The first-order concurrency estimate follows Little's Law:

```text
in_flight = arrival_rate_per_second × service_time_seconds
provisioned_slots = ceil(in_flight × headroom_factor)
```

For the baseline sustained peak:

```text
50 Turn/s × 45 s p95 Run duration × 1.33 headroom = 2,992.5
```

The initial worker/admission target is therefore **3,000 concurrent Runs**. A
60-second 100 Turn/s burst can add at most 3,000 queued Turns above the sustained
service rate; the global pending queue cap is 6,000 to leave recovery margin.

## 3. Workload assumptions

### 3.1 Tenants and interactive traffic

| ID | Assumption | Average | Sustained peak | Burst / hard cap | Configuration key |
|---|---|---:|---:|---:|---|
| TEN-01 | Provisioned tenants | 1,000 | 1,000 | 2,000 hard | `capacity.tenants.max` |
| TEN-02 | Daily active tenants | 250 | 400 | 600 hard | planning input |
| TEN-03 | Simultaneously active tenants | 50 | 100 | 200 hard | planning input |
| API-01 | Create-Turn rate | 5/s | 50/s for 30 min | 100/s for 60 s | `admission.turns_per_second` |
| API-02 | Create-Session rate | 0.5/s | 5/s | 20/s for 30 s | `admission.sessions_per_second` |
| API-03 | Per-tenant Turn rate | 0.05/s | 5/s | 20/s; token bucket | `tenant.turn_rate.*` |
| API-04 | Single-tenant share of global Turn traffic | <5% | 10% | 20% hard | `tenant.max_global_share` |
| SSE-01 | Concurrent SSE connections | 4,000 | 20,000 | 30,000 hard | `sse.max_connections` |
| SSE-02 | Reconnect attempts | 20/s | 500/s | 2,000/s for 30 s | `sse.reconnect_rate.*` |

SSE connection and reconnect budgets are deployment-wide totals across both
`/events` lifecycle streams and `/output` transient-output streams. A client
that opens both consumes two connections and each endpoint has its own cursor.

At 5 average Turn/s, the platform processes 432,000 Turns/day. This value is
used for storage and provider-cost projections; it must not be confused with the
50 Turn/s sustained-peak sizing point.

### 3.2 Agent execution

| ID | Dimension | Baseline | p95 / sustained peak | Hard cap | Configuration key |
|---|---|---:|---:|---:|---|
| RUN-01 | Run wall time | 12 s mean | 45 s | 120 s | `agent.max_run_duration` |
| RUN-02 | Concurrent Runs | 60 mean | 3,000 provisioned | 4,000 admitted | `agent.max_concurrent_runs` |
| RUN-03 | Steps per Run | 6 mean | 12 | 20 | `agent.max_steps` |
| RUN-04 | Attempts per Step | 1.1 mean | 2 | 3 | `agent.max_attempts_per_step` |
| RUN-05 | LLM calls per Run | 2.2 mean | 4 | 8 | `agent.max_llm_calls` |
| RUN-06 | Retrieval calls per Run | 1.6 mean | 3 | 6 | `agent.max_retrieval_calls` |
| RUN-07 | Read-only knowledge tool calls per Run | 0.35 mean | 2 | 5 | `agent.max_tool_calls` |
| RUN-08 | Durable lifecycle events per Run | 12 mean | 30 | 60 | `agent.max_events` |
| RUN-08B | Transient output chunks per Run | 20 mean | 80 | bounded by output bytes and one-hour default Redis TTL | `AGENTICRAG_REDIS_OUTPUT_TTL` |
| RUN-09 | Pending global Runs | <500 | 3,000 burst backlog | 6,000 | `queue.max_pending_runs` |
| RUN-10 | Pending Runs per tenant | <20 | 100 | 250 | `queue.max_pending_per_tenant` |

Derived sustained-peak downstream demand before retries:

| Dependency | Formula | Baseline peak |
|---|---|---:|
| LLM requests | `50 Runs/s × 2.2` | 110 requests/s |
| Retrieval requests | `50 Runs/s × 1.6` | 80 requests/s |
| Read-only knowledge tool executions | `50 Runs/s × 0.35` | 17.5 requests/s |
| Durable lifecycle event appends | `50 Runs/s × 12` | 600 events/s |
| Redis output appends | `50 Runs/s × 20` | 1,000 entries/s |
| Artifact metadata writes | `50 Runs/s × 4` | 200 rows/s |
| Core relational mutations | `50 Runs/s × 45` | 2,250 row mutations/s |

Retry budgets are separate from steady-state demand. Infrastructure planning
reserves 20% provider-call and database-write headroom for transient retries,
but retry storms are still limited by per-provider bulkheads.

At the sustained peak, a one-hour output TTL retains at most about 3.6 million
entries before stream/object overhead. Redis memory sizing must use measured
encoded entry size and fragmentation; raising output TTL requires a capacity
review rather than inheriting the general 24-hour Redis ceiling.

### 3.3 Token and context assumptions

| ID | Dimension | p50 | p95 | Hard cap | Configuration key |
|---|---|---:|---:|---:|---|
| TOK-01 | Context input per LLM call | 4,000 | 16,000 | 64,000 | `budget.max_input_tokens_per_call` |
| TOK-02 | Output per LLM call | 800 | 2,500 | 8,000 | `budget.max_output_tokens_per_call` |
| TOK-03 | Total tokens per Run | 12,000 | 48,000 | 128,000 | `budget.max_tokens_per_run` |
| TOK-04 | Retrieved chunks before rerank | 20 | 50 | 100 | `retrieval.candidate_top_k` |
| TOK-05 | Chunks injected after rerank | 6 | 12 | 20 | `retrieval.context_top_k` |
| TOK-06 | Tool-result payload injected into prompt | 8 KiB | 64 KiB | 256 KiB | `budget.max_tool_context_bytes` |

The average planning case uses 7,200 model tokens per Turn across all calls,
equivalent to 3.1104 billion tokens/day at 5 Turns/s. Provider budgets and cost
alarms must be sized from this aggregate, while runtime hard caps apply per Run.

### 3.4 Knowledge and storage

| ID | Dimension | Baseline | Growth / peak | Hard cap / policy |
|---|---|---:|---:|---|
| KB-01 | Knowledge bases per tenant | 5 | 20 | 100 |
| KB-02 | Source documents platform-wide | 5 million | 2%/month | 10 million before reshard review |
| KB-03 | Documents per tenant | 20,000 median | 100,000 default quota | 1 million approved quota |
| KB-04 | Chunks per document | 10 mean | 30 p95 | 500 safety cap |
| KB-05 | Searchable chunks | 50 million | 5 million/month | 100 million before topology review |
| KB-06 | Vector dimensions | 1,536 default | model-specific | 3,072 allowed after benchmark |
| KB-07 | Raw source objects | 5 TiB | 0.5 TiB/month | tenant quota enforced |
| KB-08 | New/updated documents | 10/s mean | 100/s batch | 500/s admitted batch |
| RET-01 | Online retrieval queries | 8/s mean | 80/s sustained | 200/s hard admission cap |

Knowledge indexing and interactive retrieval use separate worker pools and
quotas. A bulk ingestion job cannot consume the reserved interactive search
capacity. Each document and chunk is immutable by version; re-indexing creates a
new version and retires the old one according to retention policy.

### 3.5 Persistence footprint

Initial storage planning uses conservative uncompressed logical sizes:

| Data class | Planning size per Turn | Turns/day | Logical growth/day |
|---|---:|---:|---:|
| Core rows and indexes | 24 KiB | 432,000 | 9.9 GiB |
| Durable lifecycle event records | 12 × 0.8 KiB | 432,000 | 3.8 GiB |
| Artifact metadata | 4 × 1 KiB | 432,000 | 1.6 GiB |
| Prompt/retrieval/tool Artifacts | 160 KiB | 432,000 | 65.9 GiB |

The calculated PostgreSQL/object total is about 81.2 GiB/day before replicas, WAL, object version
overhead and compression. PostgreSQL hot data and object-storage retention are
therefore configured independently:

- durable lifecycle events: 7 days hot, then deletion after final-state reconciliation;
- transient assistant output: one-hour default Redis TTL only; no PostgreSQL event or Outbox row;
- detailed execution rows: 30 days hot, 90 days default retention;
- large execution Artifacts: 30 days default, tenant policy may shorten or extend;
- committed conversation Messages: tenant retention policy;
- audit records: security/compliance policy, never inferred from conversation
  retention.

## 4. Resource partitioning and fairness

Global capacity alone cannot protect a multi-tenant SaaS deployment. Admission
control uses hierarchical limits:

1. deployment-wide Run, queue, SSE and provider budgets;
2. workload-class reservations for interactive Runs and knowledge ingestion;
3. tenant token buckets and concurrent-Run caps;
4. provider-specific concurrency and retry budgets;
5. per-Run step, time, token, tool and cost budgets.

A tenant receives a stable `429` with retry guidance when its own quota is
exhausted. A global overload uses the same error family with a distinct reason
code. Quota rejection must occur before reserving irreversible provider work.

## 5. Overload and degradation targets

| Pressure level | Trigger sustained for 30 s | Required action | Semantics preserved |
|---|---|---|---|
| Normal | <70% of any hard capacity | Full configured quality | All |
| Guarded | 70–85% worker/provider capacity | Stop speculative candidate Runs; reduce ingestion concurrency | Final-answer correctness and ACL |
| Constrained | 85–95% or pending Runs >3,000 | Shed low-priority ingestion; cap retrieval candidates at 20; use configured lower-cost model only when tenant policy permits | Tenant isolation, evidence, final commit |
| Critical | >95%, queue >5,000, or dependency error budget exhausted | Reject new Runs with `429`; keep status/cancel/SSE resume/read APIs available | Existing Run recovery and reads |
| Emergency | Database durability or tenant-isolation control unavailable | Reject all mutations; do not fall back to unscoped or non-durable writes | Security and durability |

Degradation never disables tenant/ACL filtering, idempotency, final-commit CAS,
audit requirements or Agent safety caps. The system may return less optional
context, defer background work or reject early; it must not return cross-tenant
or silently uncommitted data.

Recovery hysteresis requires a pressure level to remain below the lower boundary
for five minutes before restoring the previous workload. This avoids oscillation.

## 6. Configuration contract

Currently implemented process controls are the queue pending limits, Worker
global/per-tenant concurrency and timing, Redis TTL/pool limits, PostgreSQL pool
limits, and HTTP lifecycle timeouts documented in the root README. Most dotted
keys in the planning tables—including admission QPS, SSE connection budgets,
Agent budgets, retrieval limits and tenant traffic share—are not wired into
`internal/config` yet. Until they are implemented, the corresponding overload
rows are required deployment policy, not a claim of automatic in-process
enforcement.

The implementation contract for every new numeric setting is:

- explicit unit in its name or type (`Duration`, tokens, bytes, requests/second);
- positive minimum and documented maximum;
- cross-field validation, such as burst ≥ sustained and hard cap ≥ admitted;
- safe startup failure for invalid configuration;
- logged effective value without secret material;
- metric exposing current utilization and rejection reason;
- runtime reload only when atomic and explicitly supported; otherwise restart.

No package may introduce a local capacity constant that bypasses this contract.

## 7. Validation scenarios

| Scenario | Input | Expected capacity behavior |
|---|---|---|
| Average day | 5 Turn/s for 24 h | Cost/storage projection uses 432,000 Turns |
| Sustained peak | 50 Turn/s, 45 s p95 for 30 min | ≤3,000 provisioned Run slots with 1.33 headroom |
| Burst | 100 Turn/s for 60 s | ≤3,000 incremental backlog; queue remains below 6,000 |
| Provider slowdown | LLM p95 doubles to 90 s | Admission reaches guarded/constrained mode before 4,000 Run cap |
| Tenant hotspot | One tenant sends >20% traffic | Tenant bucket rejects excess without starving others |
| SSE reconnect storm | 2,000 reconnects/s for 30 s across `/events` and `/output` | Resume is endpoint-rate-limited; no new Runs are created |
| Bulk ingestion | 500 documents/s while retrieval is at peak | Ingestion is shed first; reserved online retrieval capacity remains |
| Database pressure | Mutation capacity >95% | New Runs rejected; reads, cancel and recovery stay available |
| Invalid implemented config | global queue cap below per-tenant cap, output TTL above Redis max TTL, or invalid Worker lease/renew relationship | Process fails startup validation |

## 8. Acceptance checklist

- [x] Tenant counts and hotspot assumptions are explicit.
- [x] Average, sustained-peak, burst and hard-cap QPS are distinct.
- [x] Run concurrency is derived from arrival rate and p95 service time.
- [x] Token, document, chunk, SSE, persistence and growth assumptions are covered.
- [x] Retry headroom, tenant fairness and queue bounds are included.
- [x] Degradation actions and invariants are explicit.
- [x] Every operational limit is mapped to configuration or a planning input.
- [x] Boundary scenarios reconcile the model without relying on average latency.

## 9. Revalidation triggers

Re-run capacity review when any of the following changes by more than 25%:
arrival rate, p95 Run duration, calls per Run, tokens per Run, chunks per document,
Artifact bytes per Turn or tenant concentration. A provider/model change and any
new conversation concurrency policy always trigger review, regardless of size.
