# Release criteria

## Every release
- [ ] CI green on the release commit: lint, unit (`-race`), integration, gosec, govulncheck, gitleaks
- [ ] Release artifacts built by `release.yml` with `SHA256SUMS`
- [ ] CHANGELOG entry with release notes and **known limitations**
- [ ] Compatibility matrix updated (DEPENDENCY_MATRIX.md)
- [ ] Security review of changes touching restore, cleanup, credentials or `Decide()`
- [ ] Installation instructions re-verified on a clean machine
- [ ] Marked as pre-release while < 1.0

## `v0.1.0-alpha` — PostgreSQL recovery proof
| Criterion | Status (2026-11-09) |
|---|---|
| All 14 M0 acceptance scenarios pass in real integration tests | ✅ locally; ✅ CI (disk-full ENOSPC case NOT RUN on runners, pre-flight case PASS) |
| Corrupted / non-restorable backups never VERIFIED | ✅ unit + integration tests |
| No secrets in logs/files | ✅ integration test |
| CI run on GitHub | ✅ run #3 green (38b2b3f) |
| govulncheck | ✅ clean in CI run #3 |
| Release artifacts + checksums | ⬜ |
| Repository public-readiness review (secrets, history, licenses) | ✅ gitleaks + license audit on the branch |

The alpha must not be advertised as production-ready.

## `v1.0.0` additionally requires
Load tests with large databases, documented recovery scenarios, an external security audit,
upgrade tests across metadata schema versions, and a stable CLI/config contract.
