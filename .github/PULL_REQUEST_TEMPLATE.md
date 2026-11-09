## Summary

<!-- What does this change and why? Link issues. -->

## Safety checklist

- [ ] No code path can mark a backup `VERIFIED` without a real isolated restore and passing mandatory checks
- [ ] No restore/write path targets an existing (non-sandbox) database or volume
- [ ] Docker resources created are labeled `io.unthrecover.managed=true` + run label; cleanup only removes those
- [ ] No secrets in logs, errors, reports, manifests, metadata or process arguments
- [ ] No shell string concatenation with user input

## Tests

- [ ] `make check` (vet, gofmt, staticcheck, unit tests with `-race`)
- [ ] `make test-integration` — or state why it was NOT RUN
- [ ] New behaviour has tests (including the failure path)

## Documentation

- [ ] CLI reference / docs updated to match the actual behaviour
- [ ] CHANGELOG.md updated
