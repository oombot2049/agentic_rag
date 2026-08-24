# Contributing to AgenticRAG

Thank you for improving AgenticRAG. The project is design-first because small
changes to concurrency, tenant scope or retry behavior can have system-wide
effects.

## Development workflow

1. Choose one requirement or focused issue.
2. Update or add its detailed design before implementation.
3. Add tests for success, failure, concurrency and tenant boundaries as relevant.
4. Run `make check`; run `make race` on a supported CGO host.
5. Keep commits focused and use Conventional Commit messages.

## Go conventions

- Run `gofmt`; do not hand-format Go source.
- Use short lower-case package names without underscores.
- Keep domain packages free of infrastructure SDK types.
- Accept `context.Context` at request/work boundaries; do not store it in structs.
- Wrap errors with useful operation context and preserve typed/sentinel matching.
- Prefer table-driven tests and deterministic fakes over timing-dependent sleeps.
- Never make a network call inside an application database transaction.

## Security and privacy

Never commit credentials, customer data, raw prompts or proprietary documents.
Use synthetic fixtures. Report suspected vulnerabilities through the private
process in [SECURITY.md](SECURITY.md), not a public issue.

By contributing, you agree that your contribution is licensed under Apache-2.0.
