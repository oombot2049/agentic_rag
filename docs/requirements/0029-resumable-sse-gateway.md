# Requirement P3-07: Resumable SSE Gateway

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: P1-07 Redis, P3-02 authentication, P3-06 event contract
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P3-07

## 1. Objective and storage roles

Implement `GET /v1/runs/{run_id}/events` as authorized, resumable Server-Sent
Events. PostgreSQL is the seven-day authoritative event log and cursor-retention
ledger. A tenant-scoped Redis/Valkey Stream retains a short live-tail window (at
most the configured 24-hour TTL) and is acceleration only. A Redis miss/outage
never means the durable event does not exist and never broadens authorization.

## 2. Connect, replay and live handoff

```mermaid
sequenceDiagram
    participant C as Client
    participant A as Auth middleware
    participant G as SSE Gateway
    participant P as PostgreSQL event source
    participant R as Redis live Stream
    C->>A: GET Run/events + Last-Event-ID
    A->>G: trusted tenant + sessions:read
    G->>P: authorize tenant/Run; read retained_from + high_water
    alt cursor below retention watermark
        G-->>C: 410 EVENT_CURSOR_EXPIRED
    else valid
        G-->>C: 200 event-stream headers
        loop pages through fixed high_water
            G->>P: events after cursor through high_water
            P-->>G: ordered durable page
            G-->>C: id/event/data; flush
        end
        G->>R: XREAD strictly after high_water
        loop live
            R-->>G: retained/new event
            G-->>C: sequence greater than last sent; flush
        end
    end
```

The high-water mark is captured before replay. Redis XREAD starts from that same
sequence, so an event committed between the last PostgreSQL page and live attach
is still read from the Stream. If Redis cannot cover the handoff or becomes
unavailable, the gateway polls PostgreSQL from the last sent sequence before
retrying live attach. Redis data is always strict-decoded and checked against
tenant/Run/sequence before delivery.

## 3. Cursor and delivery behavior

`Last-Event-ID` must be one canonical decimal value in `0..MaxInt64`; duplicate,
comma-folded, signed, whitespace or nonnumeric forms return
`INVALID_EVENT_CURSOR`. Replay is strictly after the cursor. Duplicate delivery
from reconnect/live overlap is suppressed when `sequence <= last_sent`; the
single-statement append contract prevents committed sequence gaps. Cursor below the explicit PostgreSQL `retained_from` watermark returns
410 rather than silently skipping history.

Response headers are `text/event-stream`, `Cache-Control: no-cache, no-transform`,
`X-Accel-Buffering: no`, `Connection: keep-alive` and `nosniff`. Each durable
event is written as `id`, `event` and one compact `data` line, followed by a blank
line and immediate flush.

## 4. Heartbeat, slow consumers and shutdown

An injected 15-second ticker emits `: heartbeat` comments only while idle; it
does not consume durable sequence. The live reader feeds a bounded per-connection
queue. If the HTTP writer/proxy is slower than that queue, the gateway sends a
best-effort `retry` hint and disconnects. It never allocates an unbounded buffer.

Request cancellation, server shutdown and client disconnect cancel replay,
Redis blocking reads, tickers and helper goroutines. Terminal Run events end the
stream after flush. Write/flush failure stops immediately.

## 5. Multi-tenant keys and retention

Redis keys use the existing typed builder:
`ar:<env>:{tenant}:events:run:<run_id>`. Stream IDs are `<sequence>-0`; payload is the
strict P3-06 JSON envelope. Publish uses XADD plus explicit EXPIRE in one pipeline
and rejects a TTL outside configured bounds. Reads request bounded batches and
blocking intervals.

PostgreSQL primary keys and all reads use `(tenant_id, run_id, sequence)`. A
stream ledger allocates sequence under row lock and records `retained_from` plus
`next_sequence`, making expiry distinguishable from legitimate sequence gaps.

## 6. Test strategy and acceptance

- encoder tests assert exact framing, injection safety and flush behavior;
- handler tests cover Last-Event-ID validation, replay/live overlap dedup,
  heartbeat, terminal close, expired cursor, auth/non-enumeration and cancellation;
- a blocking ResponseWriter test fills the bounded queue and requires disconnect;
- Redis Testcontainers tests XADD/XREAD, tenant keys, sequence IDs and TTL;
- PostgreSQL tests cover concurrent allocation, watermark expiry and ordered replay;
- Linux CI runs all tests with Docker and race detection.

- [x] Disconnect/reconnect resumes strictly after Last-Event-ID without gaps at handoff.
- [x] Overlap duplicates are suppressed and Run-local order is enforced.
- [x] Heartbeat is non-durable and slow consumers are bounded/disconnected.
- [x] Cross-tenant Run subscription is non-enumerable.
- [x] Redis TTL is explicit and PostgreSQL remains authoritative.
