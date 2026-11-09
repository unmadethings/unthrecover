# Contributing to Unth Recover

Thank you for helping make recoveries provable. Please read the
[Code of Conduct](CODE_OF_CONDUCT.md) first.

## Ground rules (non-negotiable)
1. Nothing may mark a backup `VERIFIED` except `verify.Decide()` after a real isolated restore.
2. No code path may write to, restore into or delete a resource Unth Recover did not create
   and label in the current run.
3. No secrets in logs, errors, reports, manifests, metadata or process arguments.
4. No shell invocation; never build commands by string concatenation.
5. Don't claim support for a feature that isn't implemented and tested.

## Development setup
Requirements: Go 1.26+, Docker, PostgreSQL 16 client tools, Linux.

```bash
make check
```

```bash
make test-integration
```

`make check` runs vet, gofmt, staticcheck and unit tests with `-race`.
`make test-integration` drives the real binary against real PostgreSQL containers; it only
removes containers it labeled `io.unthrecover.test=<session>`. Never run it against a Docker
daemon that hosts production workloads you are not allowed to disturb.

## Pull requests
- Branch from `main`, keep PRs focused, include tests for both success and failure paths.
- Update `docs/` and `CHANGELOG.md` when behaviour changes; docs must match the CLI.
- Fill in the PR template's safety checklist. If you could not run integration tests,
  say so explicitly — do not mark them as passed.
- Commits: imperative mood, explain *why*.

## Reporting issues
Bugs and features: GitHub issues (templates provided). Security: see [SECURITY.md](SECURITY.md).

## License
By contributing you agree that your contributions are licensed under the Apache License 2.0.
