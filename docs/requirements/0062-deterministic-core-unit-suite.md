# Requirement P8-01: Deterministic Core Unit Suite

- Status: Complete
- Priority: P0
- Owner: QA / Go Backend
- Last reviewed: 2026-08-23
- Depends on: P4-10 Final Commit, P5-08 Retrieval Audit, P6-06 Built-in Tools
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P8-01

## 1. Objective

Close meaningful branch gaps in domain aggregates, Agent state/budget, Context
budgeting and idempotency without chasing one repository-wide percentage. Tests
must be deterministic, hermetic and safe under random order and the race gate.

## 2. Coverage policy

The core gate tracks behavior, not getters or generated percentages alone:

- every Run status, including `WAITING_APPROVAL`, participates in the complete
  transition matrix and illegal transitions leave state unchanged;
- public Run approval, cancellation and success/selection workflows execute;
- Step and Attempt success, failed and unknown outcomes are tied to the last
  owned Attempt and reject stale/cross-owned objects;
- Turn completion/failure/cancellation and retry behavior use fixed clocks;
- budget/context property tests use explicit seeds and print the seed on failure;
- idempotency tests cover replay, conflict, in-progress and final-failure
  decisions without wall-clock timing.

Adapter code that requires PostgreSQL is assessed through integration tests;
its hermetic statement coverage is not used to dilute or inflate the core gate.

## 3. Determinism contract

Core tests may not use package-global random generators, `time.Now`, sleeps or
poll-until-deadline loops. Property tests use local `rand.Rand` instances with a
committed seed. All timestamps are UTC fixtures. Concurrency tests synchronize
through channels/wait groups and use timeouts only as deadlock guards.

CI executes uncached shuffled tests on both supported Go versions and the full
race suite on Linux. A ten-repeat local gate detects order coupling before
commit.

## 4. Acceptance checklist

- [x] Domain/Agent/budget/Context/idempotency critical branches are covered.
- [x] Run approval state is included in the exhaustive transition matrix.
- [x] Core property tests use fixed local random sources.
- [x] Core tests have no wall-clock or sleep dependence.
- [x] Full shuffled, repeat, vet, static and dual-toolchain gates pass.
