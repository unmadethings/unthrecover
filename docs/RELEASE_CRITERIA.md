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
| All 14 M0 acceptance scenarios pass in real integration tests | ✅ locally; ✅ CI on PostgreSQL 12–17 (incl. disk-full ENOSPC) |
| Corrupted / non-restorable backups never VERIFIED | ✅ unit + integration tests |
| No secrets in logs/files | ✅ integration test |
| CI run on GitHub | ✅ run #3 green (38b2b3f) |
| govulncheck | ✅ clean in CI run #3 |
| Release artifacts + checksums | ✅ built by release.yml; verified on a clean runner and anonymously |
| Published | ✅ 2026-11-09 (pre-release); notes updated 2026-11-10 |
| Repository public-readiness review (secrets, history, licenses) | ✅ gitleaks + license audit on the branch |

The alpha must not be advertised as production-ready.

## `v0.2.0-alpha` — self-hosted Community Edition + web panel preview
| Criterion | Status (2026-11-10) |
|---|---|
| CI green on the release commit (lint, `-race`, integration PostgreSQL 12–17, packaging, gosec, govulncheck, gitleaks) | ✅ run #18 (b5c73ce) |
| Release artifacts (binaries, `.deb`, SHA256SUMS, multi-arch image) built by release.yml | ✅ release run #3 |
| Clean-machine test of the published release (install, recovery proof, corruption, scheduled run + retention, panel login, `.deb` on Ubuntu, GHCR image) | ✅ publish-release run #4 |
| Defects found during release fixed | ✅ asset names vs SHA256SUMS (9ef5336) |
| Published as pre-release | ✅ 2026-11-10 |
| GHCR package public, private vulnerability reporting enabled | ✅ 2026-11-10 |

## `v0.3.0-beta` — Community Edition beta
| Criterion | Status (2026-11-10) |
|---|---|
| CI green on the release commit | ✅ run #29 (852f551), 11/11 jobs |
| Offsite copies proven by download after upload; tampered copies never accepted | ✅ unit + integration (SeaweedFS S3, rclone) |
| Disaster recovery path: fetch on an empty machine + isolated restore VERIFIED | ✅ `TestOffsiteS3EndToEnd` |
| Encryption mandatory for offsite copies; no secrets in configs, argv or logs | ✅ tests |
| Packages: `.deb`, `.rpm`, Compose file, image with rclone | ✅ CI (Debian, Rocky 9, Fedora 41, image) and clean runner |
| Release artifacts + clean-machine test of the published release | ✅ release run #4, publish-release run #6 |
| Published | ✅ 2026-11-10 (pre-release) |
| Real-provider tests (GCS, AWS S3, Google Drive) | ⏳ need test accounts — documented as not yet tested |

## `v1.0.0` additionally requires
Load tests with large databases, documented recovery scenarios, an external security audit,
upgrade tests across metadata schema versions, and a stable CLI/config contract.
