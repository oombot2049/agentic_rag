# Requirement P1-01: Go project foundation

- Status: Implemented
- Priority: P0
- Owner: Backend
- Last reviewed: 2026-08-23
- Depends on: [ADR-0002 technology selection](../adr/0002-technology-selection.md)
- Source: `AgenticRAG_Go_详细开发计划.xlsx`, task P1-01

## 1. Objective

Create a small, reproducible Go repository that is runnable and testable before
application behavior is added. The foundation must make package boundaries,
toolchain policy, build metadata, developer commands and generated-file rules
explicit so later requirements do not invent competing conventions.

## 2. Module and toolchain

- Module path: `github.com/agenticrag/agenticrag`.
- Language/module baseline: Go 1.26.0.
- Preferred toolchain: Go 1.27.0, updated to current supported patch by normal
  dependency maintenance.
- The module starts with the standard library only. A dependency is added only
  by the requirement that uses it, with license/provenance review.

The `go` directive controls language and module semantics; the `toolchain`
directive selects the preferred developer/CI compiler. This follows ADR-0002 and
keeps compatibility with the previous supported Go family.

## 3. Repository layout

```text
cmd/agenticrag/         executable composition root only
internal/buildinfo/     build metadata independent of application packages
internal/...            private domain/application/platform packages added later
docs/adr/               architecture decisions
docs/requirements/      requirement-level detailed designs
docs/test-reports/      evidence and validation reports
Makefile                canonical local/CI quality targets
```

Rules:

1. Package names are short lower-case words without underscores.
2. `cmd/agenticrag` wires dependencies and process behavior; domain logic never
   accumulates in `main`.
3. Repository-private code stays under `internal`. A public `pkg` directory is
   not created without a proven external-consumer contract.
4. Imports flow inward: platform/transport → application → domain. Domain does
   not import infrastructure SDKs.
5. Tests live beside the package under test unless an end-to-end fixture needs a
   dedicated later directory.
6. Generated outputs and local toolchains are ignored; sources, migrations,
   OpenAPI and test fixtures are committed.

## 4. Initial executable contract

P1-01 supplies only a process/CLI smoke surface, not the P1-05 application
bootstrap or P3 HTTP API.

```text
agenticrag version
agenticrag help
```

`version` prints deterministic build metadata. Linker variables MAY inject
version, commit and build date. Empty values normalize to `dev`, `unknown` and
`unknown`; builds do not read Git or the network at runtime.

CLI execution is structured as a testable `run(args, stdout, stderr) int`
function. `main` is the only caller of `os.Exit`. Unknown commands return exit 2
and write a safe usage error to stderr.

## 5. Build and quality commands

| Target | Behavior |
|---|---|
| `make fmt` | Apply `gofmt` to Go source |
| `make fmt-check` | Fail if tracked Go source is not formatted |
| `make test` | Run all unit/package tests |
| `make race` | Run all tests with the race detector on supported CI hosts |
| `make vet` | Run `go vet ./...` |
| `make build` | Build `bin/agenticrag` with reproducible metadata flags |
| `make check` | Run format check, vet, test and build |

The Makefile uses overridable `GO`, `VERSION`, `COMMIT` and `BUILD_DATE`
variables. CI can supply deterministic values. Local caches and binaries never
enter Git.

## 6. File and line-ending policy

- Text source is UTF-8 with LF in Git.
- PowerShell scripts use LF in Git; executable shell scripts, when added, use LF.
- Excel and other binary artifacts are marked binary.
- Editors use final newline, trim trailing whitespace and four spaces by default;
  Go formatting remains exclusively `gofmt`.
- `.env`, credentials, IDE metadata, coverage files, `bin`, `.cache` and `.tools`
  are ignored.

## 7. Testing strategy

Unit tests verify:

- default and injected build metadata normalization;
- stable one-line version rendering;
- `version`, `help`, no-argument and unknown-command CLI behavior;
- stdout/stderr separation and exit codes.

Quality gates:

```text
gofmt is clean
go test ./...
go vet ./...
go build ./cmd/agenticrag
go run ./cmd/agenticrag version
```

The race detector is mandatory in Linux CI. On Windows it requires a C compiler;
local P1-01 validation records whether that host prerequisite is present rather
than pretending the race gate ran.

## 8. Acceptance criteria

- [x] `go test ./...` passes.
- [x] `go vet ./...` passes.
- [x] the executable builds and `version` runs successfully.
- [x] directory and package names follow this document.
- [x] build metadata is deterministic and unit-tested.
- [x] formatting, line endings, ignored outputs and canonical Make targets are
  defined.
- [x] no infrastructure dependency or premature application bootstrap is added.

## 9. Follow-up ownership

P1-02 adds typed configuration, P1-03 logging, P1-04 domain/API errors, P1-05
dependency injection and process bootstrap, P1-08 CI. Those tasks extend this
foundation and must not replace its commands or package conventions without an
ADR.
