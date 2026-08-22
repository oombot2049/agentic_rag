# AgenticRAG

AgenticRAG is an open-source Go backend for durable, multi-tenant Agentic RAG.
The MVP uses linear Sessions, one Active Run per Session, bounded Agent execution,
tenant-authorized retrieval and resumable server-sent events.

The project is being implemented requirement by requirement from
`AgenticRAG_Go_详细开发计划.xlsx`. Every completed task includes a detailed design,
validation evidence and an isolated conventional commit.

## Current status

The architecture baseline is complete. The Go executable currently provides the
project smoke surface; HTTP and Agent behavior arrive in their owning plan tasks.

## Prerequisites

- Go 1.26 or newer; Go 1.27 is the preferred development toolchain.
- GNU Make for the canonical convenience targets.

## Quick start

```bash
go test ./...
go vet ./...
go run ./cmd/agenticrag version
make check
```

## Architecture invariants

- PostgreSQL is the final source of truth.
- Tenant scope is trusted server context and appears in every business boundary.
- A Turn is one logical question and may own multiple Runs; no Branch exists in
  the MVP.
- Commands use HTTP/JSON and progress uses resumable GET SSE.
- Queue, cache, object store, search, LLM, tool and MCP integrations are adapters
  behind repository-owned Go interfaces.

Start with the [MVP scope](docs/requirements/0001-mvp-scope.md),
[API contract](docs/api/0005-domain-and-api-contract.md), and
[technology ADR](docs/adr/0002-technology-selection.md).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
