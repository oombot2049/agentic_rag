# Requirement P1-06: PostgreSQL foundation

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P1-02 configuration](0008-typed-configuration.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P1-06

## 1. Decision

Use `pgx/v5.10.0` through `database/sql`. This preserves standard transaction
interfaces and exact max-open/max-idle pool controls while keeping pgx protocol
support behind `internal/platform/postgres`. `Open` configures the pool and must
complete a bounded Ping before returning; failed startup closes the pool.

## 2. Runtime contract

- DSN is revealed only inside `Open` and never formatted in errors.
- Connect and health checks use separate configured timeouts.
- Pool limits and connection lifetime come only from typed config.
- `Health` is a live Ping, not proof that migrations are current.
- `Close` is idempotent through `database/sql` semantics.
- Repositories consume the narrow `DBTX` interface; driver types do not enter
  domain/application interfaces.

## 3. Transaction helper

`WithinTx` begins with explicit `sql.TxOptions`, supplies only `DBTX` to the
callback, commits once on success and rolls back on error or panic. Callback and
rollback failures are joined; `sql.ErrTxDone` during cleanup is ignored. The
callback must contain database work only—network/provider/tool calls inside a
transaction are forbidden because they extend locks and exhaustion risk.

## 4. Migration protocol

Migration files follow `NNNNNN_name.up.sql` and matching `.down.sql`. The runner:

1. acquires a connection-scoped PostgreSQL advisory lock;
2. creates `schema_migrations` if needed;
3. loads files in strict version order and rejects duplicates/gaps/bad names;
4. stores SHA-256 of each up migration and rejects checksum drift;
5. executes each up/down file in its own transaction;
6. inserts/deletes the ledger row in that same transaction;
7. releases the lock on every exit path.

A failed migration therefore rolls back both schema/data and ledger. Down
requires a checked-in matching file and rolls back newest-first. SQL files are
trusted reviewed source, never user input.

## 5. Verification

Parser unit tests cover naming, ordering, pairing, gaps and checksum stability.
Testcontainers integration uses PostgreSQL 18.6 and verifies connection health,
pool settings, commit/rollback/panic behavior, up idempotence, checksum drift,
down order and failed-migration rollback. A missing Docker provider is reported
as a skip locally; Linux CI must make Docker mandatory.

Official dependency snapshots: pgx v5.10.0 and Testcontainers Go v0.43.0,
checked 2026-08-23. Versions are pinned in `go.mod`/`go.sum`.

## 6. Acceptance

- [x] Pool startup and health calls have bounded timeouts.
- [x] Transactions commit once or roll back, including panic paths.
- [x] Migration ledger, lock, checksum and transactional rollback are defined.
- [x] Parser unit tests pass and real PostgreSQL integration tests are present.
- [x] DSN and driver types do not cross trusted package boundaries.
