# Agent execution modes: deterministic RAG and bounded ReAct

- Status: Implemented foundation; production providers pending
- Audience: architecture review and interview walkthrough
- Last reviewed: 2026-08-26

## 1. Executive summary

AgenticRAG should support two execution modes on top of one durable execution
kernel:

1. **Deterministic RAG** follows a known pipeline: contextual query rewrite,
   retrieval, answer generation, and completion.
2. **Bounded ReAct** lets an LLM Planner repeatedly choose a structured
   read-only knowledge query, inspect the observation, and either search again or
   produce the final answer.

Both modes reuse the same Run Lease, Agent Checkpoint, Step, Attempt, budget,
Executor, and fencing mechanisms. They differ only in how the next Directive is
chosen. The first uses a deterministic Planner; the second uses a
schema-constrained LLM Planner.

```text
                         Durable Agent Machine
                    (Checkpoint / Step / Attempt)
                                  |
                    +-------------+-------------+
                    |                           |
          Deterministic Planner          ReAct Planner
                    |                           |
 QUERY_REWRITE -> RETRIEVE ->       DECIDE -> SEARCH -> OBSERVE
 GENERATE -> DONE                    ^                    |
                    |                +--------------------+
                    |                           |
                    +-------------+-------------+
                                  |
                   shared validated Executors
```

The recommended product policy is to use Deterministic RAG by default and enable
ReAct only for questions that require iterative or multi-hop retrieval.

## 2. Scope and non-goals

This document focuses only on Agent planning, execution, checkpointing, and
recovery. Queue delivery, Outbox delivery, SSE, and transient token streaming are
outside this document.

The design permits only read-only knowledge access. It does not introduce:

- arbitrary SQL execution;
- write or destructive tools;
- runtime human approval;
- persistence of private model chain-of-thought;
- an end-to-end exactly-once claim for provider calls.

## 3. Current baseline versus proposed additions

The repository already implements the common execution kernel:

- `agent.Machine.Run` as a durable Planner-Executor loop;
- Planner, Executor, Directive, and Outcome contracts;
- Checkpoint phases `READY`, `EXECUTING`, `COMPLETED`, and `FAILED`;
- transactional Step, Attempt, and Checkpoint boundaries;
- takeover through Run Lease and fencing;
- retry and unknown-outcome handling;
- budgets and the read-only `knowledge.search` tool boundary;
- retrieval packages behind interfaces.

The repository now also implements:

- `DETERMINISTIC_RAG` and `REACT` mode selection;
- strict, versioned Agent state schemas for both modes;
- a deterministic Planner router that turns every external operation, including
  ReAct model decisions, into a recoverable Directive;
- the deterministic rewrite/retrieve/generate coordinator;
- the bounded ReAct decide/search coordinator, repeated-query fingerprints,
  citation-to-observation validation, and iteration limits;
- a validated Executor router with explicit query-rewrite, retrieval,
  generation, and ReAct-decision ports;
- one shared `knowledge.QueryService` contract used by deterministic retrieval
  and the ReAct `knowledge.search` adapter.

Deployment still needs concrete LLM, retrieval, knowledge, and Artifact-backed
answer provider implementations plus Run-creation composition that supplies the
initial versioned mode state.

The existing Step kinds remain the durable coarse-grained categories. Logical
operations such as query rewrite and Agent decision can be distinguished by the
Directive operation name rather than requiring a new database status:

| Logical operation | Existing Step kind |
|---|---|
| Query rewrite | `CONTEXT` or `GENERATION`, selected consistently by the concrete Planner contract |
| Knowledge retrieval | `RETRIEVAL` |
| ReAct decision | `GENERATION` |
| `knowledge.search` | `TOOL` |
| Final answer generation | `GENERATION` |

The preferred convention is `CONTEXT/query_rewrite`,
`RETRIEVAL/knowledge_retrieve`,
`GENERATION/tool_decision`, `TOOL/knowledge.search`, and
`GENERATION/final_answer`.

## 4. Shared domain model

The four state layers must not be confused:

```text
Run
└── Agent Checkpoint
    └── Step
        └── Attempt
```

| Layer | Meaning |
|---|---|
| Run | One complete answer execution and its business lifecycle |
| Checkpoint | The resumable program counter and minimal durable Agent state |
| Step | One logical action chosen by the Planner |
| Attempt | One technical try of a Step |

A normal in-flight snapshot may be:

```text
Run.status       = RUNNING
Checkpoint.phase = EXECUTING
Step.status      = RUNNING
Attempt.status   = STARTED
```

### 4.1 Checkpoint phases

| Phase | Meaning on recovery |
|---|---|
| `READY` | There is no unresolved active Step; Planner may choose the next Directive. |
| `EXECUTING` | Step, Attempt, and Directive were committed, but the provider result was not durably completed. |
| `COMPLETED` | Planner declared the Agent loop complete; return the stored result. |
| `FAILED` | A stable terminal failure was committed; return it without replanning. |

Checkpoint Phase belongs to the Run's Agent loop. It is not a substitute for
RunStatus, StepStatus, or AttemptStatus.

### 4.2 Planner and Executor

Planner answers:

> Given the current durable Agent state, what is the next logical action, or is
> the answer complete?

It returns either `Done` or a structured Directive containing:

- Step kind and stable logical operation key;
- bounded JSON input;
- unknown-outcome retry policy.

Executor answers:

> Given a validated Directive, execute the corresponding operation and return a
> bounded Outcome.

Planner never invokes a tool directly. Executor never decides the next business
action. This split allows a fixed Planner, a rule Planner, and an LLM Planner to
reuse the same durable Machine.

### 4.3 One retrieval core, two orchestration entries

`rag.retrieve` and `knowledge.search` are different orchestration operations but
not different search implementations:

```text
Deterministic RAG Executor -----+
                               +--> knowledge.QueryService --> authorized retrieval
ReAct knowledge.search Adapter-+
```

The deterministic path chooses retrieval because its workflow requires it. The
ReAct path chooses `knowledge.search` through a model Action and therefore adds
tool-schema validation. Both paths pass trusted tenant and Run identity from the
fenced Executor invocation and converge on the same QueryService. QueryService
owns ACL enforcement and the production retrieval pipeline; model State cannot
supply or override tenant authority.

## 5. Mode A: Deterministic RAG

### 5.1 Use case

This mode serves questions with a known answer pipeline:

```text
Question + conversation history
              |
              v
       contextual query rewrite
              |
              v
       knowledge retrieval
              |
              v
       grounded answer generation
              |
              v
             Done
```

“Deterministic” means the stage order is fixed. The rewritten query and answer
are still dynamically produced.

### 5.2 State schema

The illustrative durable state is:

```json
{
  "mode": "DETERMINISTIC_RAG",
  "stage": "RETRIEVE",
  "original_question": "退款多久能到账？",
  "rewritten_query": "信用卡退款到账周期",
  "retrieval": {
    "evidence_artifact_id": null,
    "evidence_ids": []
  },
  "answer_artifact_id": null
}
```

Large chunks and prompts do not belong in the Checkpoint. The Checkpoint stores
bounded identifiers, hashes, scores, and Artifact references.

### 5.3 Deterministic Planner transitions

```text
START
  -> QUERY_REWRITE
  -> RETRIEVE
  -> GENERATE
  -> DONE
```

Conceptually:

```go
switch state.Stage {
case "START":
    return queryRewrite(state)
case "RETRIEVE":
    return retrieve(state.RewrittenQuery)
case "GENERATE":
    return generate(state.Retrieval.EvidenceIDs)
case "DONE":
    return Plan{Done: true}
}
```

This is Planner-Executor, but not ReAct: no model call decides which stage comes
next.

### 5.4 Query rewrite

The query-rewrite Executor receives:

- current user question;
- bounded recent conversation context or a durable summary;
- locale and product terminology;
- no authority-bearing tenant fields from model output.

It returns one canonical retrieval query and optionally typed filters. The
original question remains immutable and is still used during answer generation.
Rewrite failure can either fail the Run or fall back to the original question by
explicit policy; it must not silently broaden tenant or ACL scope.

### 5.5 Retrieval

Retrieval applies server-controlled tenant, knowledge-base, ACL, version, and
retention filters before ranking. Model-produced input may supply only allowed
query and filter fields. Results are validated again after retrieval.

A common two-stage retrieval configuration is:

```text
keyword recall -------+
                      +--> fusion --> candidate top N
vector recall --------+
```

Retrieval returns a bounded evidence set that fits the generation context budget.

### 5.6 Generation

Generation receives:

- the original question;
- bounded conversation context;
- the rewritten query for traceability;
- the final evidence set returned by retrieval;
- citation identifiers resolved by the server.

The answer is accepted only if its citations resolve to the authorized evidence
set. The model cannot trigger an unscoped lookup by inventing a citation.

### 5.7 Strengths and limits

Strengths:

- predictable latency and cost;
- straightforward testing and capacity planning;
- low loop and prompt-injection risk;
- simple recovery because every stage has one known successor.

Limits:

- one rewrite/retrieval pass may miss multi-hop evidence;
- the model cannot dynamically reformulate after inspecting poor results;
- special questions may require mode escalation.

## 6. Mode B: bounded ReAct Agent

### 6.1 Use case

This mode is for exploratory or multi-hop questions. An LLM receives the user
question, bounded context, an allowed knowledge schema, and the
`knowledge.search` JSON contract. It repeatedly returns either a tool call or a
final answer:

```text
LLM decision
   |
   +--> knowledge.search(query, filters, top_k)
   |           |
   |           v
   |       observation
   |           |
   +-----------+
   |
   +--> final answer
```

This is a bounded ReAct strategy implemented inside the LLM Planner. The
system stores structured Action and Observation data, not private chain-of-thought.

### 6.2 Structured decisions

Tool decision:

```json
{
  "decision": "CALL_TOOL",
  "tool": "knowledge.search",
  "arguments": {
    "query": "信用卡退款到账周期",
    "filters": {"product": ["credit_card"]},
    "top_k": 12
  }
}
```

Final decision:

```json
{
  "decision": "FINAL",
  "answer": "……",
  "citations": ["chk_123", "chk_456"]
}
```

Unknown fields, unknown tools, invalid filters, oversized queries, and
out-of-range limits fail closed before Executor invocation.

### 6.3 SQL and schema safety

The preferred design does not let the model execute arbitrary SQL. The model
generates a typed retrieval DSL which the server compiles into parameterized
queries after injecting trusted scope:

```json
{
  "query": "退款到账周期",
  "filters": {
    "product": ["credit_card"],
    "region": ["CN"]
  },
  "top_k": 10
}
```

If direct SQL is ever required, it needs a separate security design: SQL AST
parsing, one `SELECT`, read-only views and credentials, RLS, forced tenant
predicates, statement timeout, row/byte/cost limits, forbidden functions and
catalogs, and post-result identity validation. Prompt instructions alone are not
a security boundary.

### 6.4 Tool execution

`knowledge.search` is a composite read-only operation:

```text
validate arguments and permissions
        -> retrieve candidates
        -> enforce result limits
        -> return typed observation
```

### 6.5 Durable iteration model

For clearest recovery semantics, LLM decision and knowledge execution are
separate Steps:

```text
READY
  -> EXECUTING(GENERATION/tool_decision)
  -> READY(pending_action = knowledge.search)
  -> EXECUTING(TOOL/knowledge.search)
  -> READY(observations += evidence references)
  -> EXECUTING(GENERATION/tool_decision)
  -> ...
  -> READY(final_answer = reference)
  -> COMPLETED
```

They are separate because the LLM and knowledge provider have different
timeouts, retry rules, budgets, and failure classifications.

### 6.6 Loop termination

The loop ends when any terminal condition occurs:

- Planner returns a valid final answer;
- maximum Agent iterations is reached;
- maximum tool or LLM calls is reached;
- Step or Attempt limit is reached;
- token, cost, evidence-byte, or wall-time budget is exhausted;
- the Run deadline expires;
- a permanent provider failure occurs;
- an unknown outcome is not retryable;
- cancellation or lease loss cancels the execution context.

Recommended independent limits include:

```text
max_agent_iterations
max_tool_calls
max_llm_calls
max_steps
max_attempts_per_step
max_total_tokens
max_tool_result_bytes
max_evidence_chunks
max_wall_time
max_estimated_cost
```

### 6.7 Repeated-query control

Every canonical tool request receives a fingerprint:

```text
SHA-256(canonical query + canonical filters + knowledge scope + index version)
```

If the Planner repeats the same request, the system reuses the prior observation
or returns a structured `duplicate_query` observation. Repeated iterations with
no new evidence consume a small fixed allowance and then terminate. This avoids
loops such as:

```text
query A -> empty -> query A -> empty -> query A -> empty
```

### 6.8 Evidence-sufficiency policy

The LLM may propose that evidence is sufficient, but the server validates:

- at least one authorized evidence item exists when policy requires grounding;
- citations refer only to observed evidence IDs;
- evidence and answer sizes remain bounded;
- no unresolved pending tool action remains;
- the final decision satisfies the response schema.

An optional deterministic evaluator can request one more iteration when coverage
is insufficient, but it must remain inside the same global budgets.

## 7. Checkpoint and recovery

### 7.1 Durable boundaries

The Machine does not restore a goroutine or instruction pointer. It resumes from
business-semantic boundaries:

```text
Checkpoint A: what is about to execute
       |
       | external provider call
       v
Checkpoint B: what durably completed
```

Before execution, one PostgreSQL transaction:

1. creates the Step;
2. changes it to `RUNNING`;
3. creates a `STARTED` Attempt;
4. stores an `EXECUTING` Checkpoint.

After success, another transaction:

1. changes Attempt to `SUCCEEDED`;
2. changes Step to `SUCCEEDED`;
3. stores updated bounded Agent state;
4. advances Checkpoint to the next `READY`.

Provider calls never run inside these database transactions.

### 7.2 Crash matrix

| Crash point | Durable state | Recovery |
|---|---|---|
| Before the `EXECUTING` transaction commits | Previous `READY` | Replan the same next Step. No partial Step/Attempt exists. |
| After `EXECUTING`, before provider request | `EXECUTING` and `STARTED` | Treat the old Attempt as unknown and apply retry policy. |
| After provider success, before completion commit | Still `EXECUTING` and `STARTED` | Outcome is unknown; retry only when the Directive permits it. |
| After completion commit | Next `READY` | Continue with the next Step; do not repeat the completed Step. |
| After `COMPLETED` or `FAILED` | Terminal Checkpoint | Return the stable terminal result. |

For the read-only knowledge tool, an unknown call can be retried within budget
without creating an external mutation. LLM generation may also be retried when
policy permits, but the system does not claim that the provider received exactly
one request.

### 7.3 Takeover and fencing

When the old Worker stops renewing, a replacement Worker:

1. waits for the Run Lease to expire;
2. acquires the Run using execution-version CAS;
3. receives a larger `lease_token`;
4. loads and validates the Checkpoint;
5. resumes according to its Phase.

Every critical write checks tenant, Run, lease owner, lease token,
execution version, and lease expiry. A displaced Worker may still be physically
running, but its stale token cannot complete an Attempt, Step, Checkpoint, or Run.

Checkpoint `step_no` also cannot move backwards, and Step/Attempt row versions
prevent lost updates inside the current lease generation.

## 8. Mode selection

Recommended routing policy:

```text
incoming question
       |
       v
deterministic complexity classifier
       |
       +--> ordinary / single-hop --> DETERMINISTIC_RAG
       |
       +--> multi-hop / exploratory --> REACT
```

The classifier must be deterministic or tightly bounded. A safer alternative is
progressive escalation:

```text
run Deterministic RAG
       |
       +--> sufficient evidence --> generate answer
       |
       +--> insufficient evidence --> enter bounded ReAct mode
```

Escalation preserves all consumed budgets. It does not reset the Run deadline,
Step count, token use, or cost ledger.

## 9. Comparison

| Dimension | Deterministic RAG | Bounded ReAct |
|---|---|---|
| Next action | Fixed/rule Planner | LLM Planner |
| Retrieval passes | Normally one | Zero to bounded many |
| Latency/cost | Predictable | Higher variance |
| Testing | Stage contract and golden tests | State-machine, schema, loop, and adversarial tests |
| Prompt-injection exposure | Lower | Higher because observations influence later actions |
| Best use | FAQ, policy, single-hop grounding | Multi-hop and exploratory research |
| Failure mode | Known stage failure | Loop, repeated query, or premature final answer |

## 10. Observability and evaluation

Record bounded metadata for each Run and Step:

- execution mode and stage/operation;
- iteration, Step, Attempt, LLM, retrieval, and tool counts;
- query fingerprints, not unrestricted prompts in ordinary logs;
- retrieved candidate and accepted evidence counts;
- evidence IDs and scores through audited storage;
- termination reason and consumed budgets;
- retry class and unknown-outcome count.

Evaluation should cover:

- rewrite quality with conversational ellipsis and coreference;
- retrieval recall, precision, and accepted evidence count;
- answer faithfulness and citation validity;
- multi-hop completion rate;
- repeated-query suppression;
- termination under every hard limit;
- crash recovery at every durable boundary;
- stale-Worker fencing;
- prompt injection attempting to widen tenant scope or invoke another tool.

## 11. Remaining implementation sequence

1. Compose production query-rewrite, LLM, retrieval, knowledge, and Artifact providers.
2. Persist the initial mode state as part of Run execution setup.
3. Add mode selection or progressive escalation at the application boundary.
4. Add production evidence-sufficiency policy and evaluation datasets.
5. Run provider recovery, fencing, budget, adversarial, and load tests before enabling the
   dynamic mode by default.

## 12. Interview walkthrough

A concise explanation is:

> The system has one durable Planner-Executor kernel and two planning strategies.
> For normal questions, a deterministic Planner advances query rewrite,
> retrieval, and generation. For multi-hop questions, a
> schema-constrained LLM Planner repeatedly chooses the read-only
> `knowledge.search` tool or returns a final answer. In both modes, every logical
> action is a Step and every technical retry is an Attempt. Before a provider
> call, we atomically persist the Step, Attempt, and an `EXECUTING` Checkpoint;
> after success, we atomically complete them and advance to `READY`. A replacement
> Worker gets a larger fencing token and resumes from that Checkpoint, while the
> stale Worker cannot commit. The dynamic loop has hard limits on iterations,
> tool calls, tokens, evidence, time, and cost, and repeated queries are detected
> by canonical fingerprints. We guarantee recoverable state transitions and
> bounded at-least-once provider execution, not provider-level exactly once.

Common follow-up answers:

**Is this ReAct or Planner-Executor?**

The execution architecture is Planner-Executor. ReAct uses a bounded
Reason-Action-Observation strategy inside the LLM Planner; Deterministic RAG uses
a fixed Planner.

**Why not let the model execute SQL?**

Schema in a prompt is guidance, not authorization. The model emits a typed DSL;
the server injects tenant/ACL scope and compiles a bounded parameterized query.

**What happens if the Worker crashes after the provider responds?**

The Checkpoint still says `EXECUTING`, so the outcome is unknown. The read-only
knowledge call can be retried within budget. Fencing prevents the old Worker from
later committing with its stale token.

**Does Checkpoint guarantee exactly once?**

No. It guarantees that committed logical Steps do not move backward or get
committed by a stale Worker. Provider requests in the gap between the two durable
transactions may repeat.

**Why not use ReAct for every question?**

Most knowledge questions have a predictable path. A fixed pipeline is cheaper,
faster, safer, and easier to evaluate; dynamic iteration is reserved for cases
where its extra retrieval capability is worth the variance.
