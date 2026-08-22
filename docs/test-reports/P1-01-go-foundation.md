# P1-01 validation report: Go project foundation

- Result: Passed
- Date: 2026-08-23
- Go: `go1.27.0 windows/amd64`

## Toolchain provenance

The portable toolchain was selected from the official `go.dev` JSON release
feed and stored under ignored `.tools`:

- archive: `go1.27.0.windows-amd64.zip`
- size: 78,924,880 bytes
- official and verified SHA-256:
  `f0c0a0d33ba94f4d2c5dbc887334ce678b21813504ddb3aafcb06e60a5a667c4`

The archive and extracted toolchain are not part of the repository.

## Commands and results

```powershell
go fmt ./...
go mod tidy
go test -cover ./...
go vet ./...
$env:CGO_ENABLED = '0'
go build -buildvcs=false -trimpath -ldflags '<test metadata>' -o bin/agenticrag.exe ./cmd/agenticrag
bin/agenticrag.exe version
bin/agenticrag.exe help
```

Results:

| Check | Result |
|---|---|
| `gofmt` | Pass; no unformatted file |
| Module tidy | Pass; standard-library-only module needs no `go.sum` |
| `go test ./...` | Pass |
| `go test -shuffle=on -count=20 ./...` | Pass |
| `cmd/agenticrag` statement coverage | 90.9% |
| `internal/buildinfo` statement coverage | 100.0% |
| `go vet ./...` | Pass |
| CGO-disabled build | Pass |
| Injected build metadata | Pass; exact expected one-line output |
| Help command | Pass; usage rendered to stdout |
| Package-name/list validation | Pass; two expected packages, valid lower-case names |
| `git diff --check` | Pass |

The validated injected output was:

```text
agenticrag v0.1.0-test (commit deadbeef, built 2026-08-23T00:00:00Z)
```

## Race detector

The Windows host has no C compiler. `go test -race ./...` first correctly
reported that race requires CGO; with `CGO_ENABLED=1`, it reported
`C compiler "gcc" not found`. This is a host prerequisite limitation, not a test
pass. P1-08 must run `go test -race ./...` on Linux CI and make failure blocking.

## Conclusion

P1-01 acceptance criteria are met: the repository has a conventional Go module
and layout, deterministic build metadata, tested runnable CLI, canonical quality
targets and open-source foundation files without premature infrastructure or
application dependencies.
