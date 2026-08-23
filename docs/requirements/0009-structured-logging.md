# Requirement P1-03: Context-aware structured logging

- Status: Implemented
- Priority: P1
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [P1-01 Go foundation](0007-go-project-foundation.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P1-03

## 1. Objective

Provide one JSON logging component that automatically correlates an ingress
attempt with its tenant and AgenticRAG domain execution, while preventing common
credential and user-content leaks. The implementation builds on standard-library
`log/slog` and remains compatible with OpenTelemetry context propagation added
later.

## 2. Correlation model

The following attribute names are reserved and supplied only from context:

| Field | Meaning | Required at |
|---|---|---|
| `tenant_id` | authenticated tenant isolation boundary | authenticated request |
| `request_id` | one ingress HTTP attempt | HTTP/SSE ingress |
| `trace_id` | distributed causal trace | traced request/worker |
| `session_id` | linear conversation | session-scoped operation |
| `turn_id` | logical user question | Turn/Run operation |
| `run_id` | one Agent execution candidate | Run operation |

`WithCorrelation` merges non-empty fields over existing context, enabling
middleware to add tenant/request/trace first and application code to add domain
IDs later. Blank values are omitted. A log record cannot spoof reserved fields:
caller-supplied attributes with these keys are discarded, then trusted context
values are appended.

Callers MUST use `DebugContext`, `InfoContext`, `WarnContext`, `ErrorContext` or
`LogAttrs`. Context-free `slog` methods cannot carry request correlation and are
reserved for process lifecycle events.

## 3. Output contract

`NewJSON` emits one JSON object per line through a concurrency-safe `slog`
handler. Standard fields are `time`, `level` and `msg`; optional process fields
are `service`, `environment` and `version`. Messages are stable snake-case event
names such as `run_started`, not interpolated prose or user content.

The handler is composable through `NewContextHandler`, so later tracing/export
adapters can preserve the same context and redaction behavior. The handler does
not perform network I/O and does not own log rotation.

## 4. Sensitive-data policy

The handler delegates to the [P7-04 unified redactor](0058-unified-sensitive-data-redaction.md).
Exact sensitive keys and credential suffixes are replaced by `[REDACTED]`,
including nested groups and logger-scoped attributes. String values, messages
and field names also receive content detection. The protected set includes
authorization/cookie credentials, password/secret/API-key/DSN fields, Prompt
content, general content, tool arguments and retrieval content. Safe metadata
keys such as `prompt_hash`, `prompt_bytes`, `token_count`, `artifact_id` and
`document_id` remain visible.

The handler cannot infer that an arbitrary message string is secretly a Prompt.
Therefore:

1. `msg` MUST be a static event name.
2. Prompt, message, retrieval and tool payloads MUST NOT be passed as messages.
3. Log only length, hash, media type, count, provider status and Artifact IDs.
4. `config.Secret.Reveal` may be used only to construct its owning client, never
   as a log argument.

## 5. Failure and performance behavior

- Logging never changes request/domain success behavior.
- The handler clones each record before filtering, preserving safe caller data.
- Empty context fields add no attributes.
- Level filtering occurs before context enrichment.
- The standard handler serializes writes; the component has no global logger or
  mutable correlation singleton, preventing cross-request contamination.
- Log backpressure/output failure handling is delegated to the configured
  writer and deployment collector. A later observability task owns drop metrics.

## 6. Package API

```text
internal/observability/log
  Correlation
  WithCorrelation / CorrelationFromContext
  NewContextHandler
  NewJSON and Options
  reserved-field and sensitive-field filtering
```

Package name is `log`; import aliases such as `applog` are recommended at
composition roots to avoid confusion with the standard `log` package.

## 7. Test strategy

Unit tests decode emitted JSON and verify all six fields, incremental context
merge, omission of blanks, reserved-field anti-spoofing, top-level and nested
redaction, logger-scoped redaction, safe metadata preservation, level filtering
and concurrent request isolation. Full repository format, test, vet and build
gates also run.

## 8. Acceptance criteria

- [x] Context-aware calls automatically carry available tenant/request/trace/session/turn/run IDs.
- [x] Reserved correlation fields cannot be overridden by record attributes.
- [x] Credential, Prompt, content, tool and retrieval payload attributes redact.
- [x] Safe content metadata remains observable.
- [x] JSON output, filtering, merge and concurrent isolation are unit-tested.
- [x] No complete Prompt or secret is present in test output.

## 9. Follow-up ownership

P1-05 installs the logger during bootstrap after configuration validation. P3
middleware generates/extracts request and trace IDs and attaches tenant/session
scope. P7 adds OpenTelemetry trace/span correlation, metrics, audit separation,
sampling and production collector behavior. P7-04 supplies the tenant-aware
policy engine while preserving this handler's strict process-level fallback.
