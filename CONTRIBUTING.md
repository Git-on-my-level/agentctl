# Contributing to agentctl

Thanks for helping improve agentctl. The project is in public preview, so small,
well-evidenced changes are easier to review than broad compatibility promises.

## Before starting

- Search existing issues and pull requests.
- For a new adapter, journal-format change, external protocol, or major command,
  open a design issue before implementation.
- Do not include credentials, private prompts/results, native session files,
  local machine paths, or private infrastructure examples in issues, fixtures,
  commits, or pull requests.
- Use [Security](SECURITY.md), not a public issue, for vulnerabilities.

## Development

Use the Go version declared by `go.mod` or a newer supported patch release,
Python 3 for contract checks, and a POSIX shell. Then run:

```bash
make hooks
make ci
go test -race ./...
```

`make hooks` selects the tracked `.githooks` directory for this checkout. Its
pre-push gate runs a bounded subset of the same formatting, vet, test, schema,
link, script, and portable-asset checks used by CI. Distribution validators,
builds, race tests, and vulnerability scanning remain explicit or hosted CI
checks.

Live adapter tests are optional because they may consume provider resources or
alter a native harness cache. State any live testing performed and never commit
its output.

Changes should preserve these boundaries:

- native CLIs own authentication, permissions, models, and session semantics;
- agentctl stores normalized metadata and explicit bounded results, not raw
  transcripts or reasoning;
- JSON and errors remain stable, typed, and machine-readable;
- mutations use exact references, plans where available, and idempotency;
- read-only commands do not hide network or filesystem writes; and
- optional integrations do not become prerequisites for standalone use.

Update command help, schemas, fixtures, and documentation with behavior changes.
Do not claim a backend capability that is only inferred or untested.

## Pull requests

A pull request should explain:

1. the user-visible problem and chosen boundary;
2. compatibility and data-migration effects;
3. security, privacy, and retention implications;
4. tests run, including relevant race or disposable-home tests; and
5. known limitations or follow-up work.

By submitting a contribution, you agree that it is licensed under this
repository's [Apache License 2.0](LICENSE), as described in section 5 of that
license. A separate contributor license agreement is not currently required.

Community participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).
