# Test results — M0

Run date: 2026-11-09. Environment: Linux 6.18 x86_64 (ephemeral cloud dev container), Go 1.26.10,
Docker Engine 29.9.2 (API 1.56, started inside the container for this run), pg_dump/pg_restore 16.15,
source images `postgres:16-alpine` (16.15) and `postgres:17-alpine`, restore images
`postgres:16-alpine` (suite) and `postgres:16` (manual run).

Status values: **PASS** = executed and passed · **FAIL** · **NOT RUN** = not executed (reason given).

## M0 acceptance matrix

| # | Scenario | Expected | Result | Test |
|---|---|---|---|---|
| 1 | Healthy PostgreSQL backup | PASS | **PASS** | `TestHealthyBackupIsVerified` |
| 2 | Successful isolated restore | PASS | **PASS** | same (restore step + sandbox cleanup asserted) |
| 3 | Sample records match | PASS | **PASS** | row totals compared with independent `psql` counts; 7/7 tables content-hashed |
| 4 | Schema and constraint checks | PASS | **PASS** | `schema_and_constraints` step; `TestDataMismatchIsDetected` proves the comparison is live |
| 5 | Checksum corruption | FAIL | **PASS** (FAILED/CHECKSUM_MISMATCH, no restore) | `TestChecksumCorruptionFails` |
| 6 | Missing backup artifact | FAIL | **PASS** (ARTIFACT_MISSING) | `TestMissingArtifactFails` |
| 7 | Wrong PostgreSQL credentials | FAIL | **PASS** (AUTHENTICATION_FAILED) | `TestWrongCredentialsFailWithoutLeakingSecrets` |
| 8 | Process interrupted during restore | FAIL + safe cleanup | **PASS** (SIGINT → CANCELLED, exit 130, resources removed; SIGKILL → `unth cleanup` reconciles) | `TestInterruptDuringRestoreCleansUp`, `TestKillDuringRestoreThenCleanup` |
| 9 | Insufficient disk space | Controlled FAIL | **PASS** (pre-flight and real ENOSPC on a 448 KiB tmpfs; partial files removed) | `TestInsufficientDiskPreflight`, `TestDiskFullDuringBackup` |
| 10 | Isolated environment cannot be created | FAIL | **PASS** (ENVIRONMENT_FAILED) | `TestEnvironmentCreationFailure` |
| 11 | Bad Docker access | Descriptive FAIL | **PASS** (DOCKER_UNAVAILABLE, "socket does not exist" hint) | `TestDockerUnavailable` |
| 12 | No secrets in logs | PASS | **PASS** (all stdout/stderr at debug level + every file in data dir scanned) | `TestSecretsDoNotLeak`, `TestWrongCredentials…` |
| 13 | Temporary resources cleaned up | PASS | **PASS** (per-run assertion in every test + suite-wide check: 0 leftovers) | all |
| 14 | Unsupported PostgreSQL version | Controlled error | **PASS** (real PG 17 server vs pg_dump 16; and shimmed older pg_dump) | `TestUnsupportedVersionRealServer`, `TestUnsupportedVersionViaOlderTool` |

**Key criterion — a corrupted or non-restorable backup is never VERIFIED:** PASS
(`TestConsistentlyCorruptedArtifactIsNeverVerified`: truncated archive with checksums rewritten to
match → checksum PASS but run FAILED; plus `Decide()` unit tests).

## Additional tests

| Area | Result | Notes |
|---|---|---|
| Unit tests (9 packages) | **PASS** | `make check` |
| Race detector | **PASS** | `go test -race ./...` |
| Integration suite (21 tests) | **PASS** | `UNTH_INTEGRATION_REQUIRED=1 make test-integration`, 62.9 s |
| CLI smoke tests | **PASS** | `TestCLISmoke` + `internal/cli` tests |
| Idempotency (repeat verify, repeat cleanup) | **PASS** | `TestRepeatedVerifyCreatesNewRuns`, cleanup twice |
| Cancellation | **PASS** | SIGINT test |
| Timeout | **PASS** | `TestVerifyTimeout` (1 s) |
| Invalid config | **PASS** | `internal/config` tests, CLI exit code 2 |
| Interrupted-operation recovery | **PASS** | SIGKILL + `unth cleanup` |
| Snapshot consistency under concurrent writes | **PASS** | `TestConcurrentWritesDuringBackupStillVerify` (found and fixed a real bug: sequences are non-transactional) |
| Tampering: manifest, symlinked artifact | **PASS** | `TestManifestTamperingFails`, `TestSymlinkedArtifactRejected` |
| Cleanup never deletes foreign resources | **PASS** | unit test with a filter-ignoring fake Docker Engine |
| Default Debian restore image `postgres:16` | **PASS** | manual run, VERIFIED, cleanup PASS |
| go vet / gofmt / staticcheck | **PASS** | `make lint` |
| gosec | **PASS** | 0 findings after triage (G304/G302 annotated with justification, G104 fixed) |
| gitleaks (working tree) | **PASS** | "no leaks found" |
| License review | **PASS** | all deps MIT/BSD/Apache-2.0 |
| govulncheck | **PASS** (CI run #3) | Blocked locally (HTTP 403). CI runs #1–#2 found vulnerabilities in pgx 5.7.5, x/text 0.27.0 and the Go 1.26.0 stdlib; fixed by upgrades and `toolchain go1.26.10` |
| GitHub Actions CI | **PASS** (run #3, commit 38b2b3f) | lint, race unit tests, build, govulncheck, gosec, gitleaks (full history), integration 20 PASS / 1 SKIP, leftover-resource check — https://github.com/unmadethings/unthrecover/actions/runs/42689780838 |
| Disk-full test in CI | **PASS** (from run #6) | CI pre-mounts a 448 KiB tmpfs with sudo (`UNTH_TEST_TINY_FS`); before that it was NOT RUN on runners |
| PostgreSQL version matrix | **PASS** | 12, 13, 14, 15, 16 (pg_dump 16) and 17 (pg_dump 17): full integration suite each, CI run #7 — https://github.com/unmadethings/unthrecover/actions/runs/42697171050. PG 12 first failed on a test-fixture type error (fixed) |
| Large database (opt-in, `UNTH_TEST_LARGE_ROWS=5000000`) | **PASS** (local) | 5M rows, 1,526 MB → artifact 158.5 MiB; backup 16 s; verify 32 s total, of which restore 26 s; report states the 5M-row table was row-counted, not content-hashed |
| macOS / Docker Desktop | **NOT RUN** | unsupported in M0 |

## Defects found and fixed during testing
1. `UpdateBackup` overwrote `verification_status` with an empty string (unit test).
2. `"char"` catalog columns made fingerprint SQL ambiguous under `search_path=pg_catalog` (first E2E run).
3. Sequence values compared for equality — false failure under concurrent writes; now monotonic (integration test).
4. Interrupt test initially signalled during `pg_restore --list`, not the restore; harness fixed to target the real restore.
5. Vulnerable dependencies (pgx SQL-injection placeholder confusion, x/text DoS) found by govulncheck in CI; upgraded (minimum Go is now 1.26).
6. CI run #2: Go 1.26.0 standard-library vulnerabilities (fixed in 1.26.1+); `toolchain go1.26.10` pinned in go.mod.
7. PostgreSQL 12 rejected an implicit numeric→bigint return in a test-fixture SQL function (fixture fixed with an explicit cast).
8. Large-DB test logged a 0 s restore time because it searched compact JSON for a key with a trailing space; now decodes the report.

## M1 (2026-11-10) — local and CI (run #11, PostgreSQL 12–17 matrix + installer tests: https://github.com/unmadethings/unthrecover/actions/runs/42729470008)

| Area | Result | Test |
|---|---|---|
| Webhook delivery, HMAC signature, Slack format, on=failure skip, no redirects, URL policy, URL/secret redaction | **PASS** | `internal/notify` |
| Retention planner: keep_last/daily/weekly, max_age, newest VERIFIED and in-progress always kept | **PASS** | `internal/retention` |
| `unth run` end-to-end ×3 with signed webhook (local receiver), prune of the oldest, EXPIRED cannot be verified, no secret leaks | **PASS** | `TestRunJobVerifiesPrunesAndNotifies` |
| Failed verification → prune skipped, Slack failure message | **PASS** | `TestRunNotVerifiedSkipsPruneAndNotifiesFailure` |
| Notification failure → exit 4 | **PASS** | `TestRunNotificationFailureExitCode` |
| Overlapping run refused by lock | **PASS** | `TestRunRefusesOverlappingJob` |
| Volume backup VERIFIED (owners, modes, symlinks, binary data, sticky dir) | **PASS** | `TestVolumeBackupIsVerified` |
| `offline` refuses running writer, container untouched | **PASS** | `TestVolumeOfflineRefusesRunningWriter` |
| `stop` stops/restarts only running mounters, audited | **PASS** | `TestVolumeStopPolicyStopsAndRestartsWriters` |
| Tampered content and `..` traversal archive with rewritten checksums never VERIFIED; traversal never reaches Docker | **PASS** | `TestVolumeTamperedArchiveIsNeverVerified` |
| Volume `unth run`, missing volume, type mismatch | **PASS** | `TestVolumeRunJobAndMissingVolume` |
| Unsafe tar entries (absolute, `..`, outside root, hard link out, duplicate, limits) | **PASS** | `internal/volume` |
| Installer: install, dry run, tampered binary, missing checksum, invalid version, http base URL | **PASS** | `make test-install` |
| systemd units | **PASS** (syntax) | `systemd-analyze verify` (only error: binary not installed in sandbox); not run under a real systemd PID 1 |

## Release v0.1.0-alpha — clean-machine test (2026-11-10)

| Check | Result | Evidence |
|---|---|---|
| Published assets download + `sha256sum -c SHA256SUMS` (amd64, arm64) | **PASS** | publish-release run #2 — https://github.com/unmadethings/unthrecover/actions/runs/42826860958 |
| `scripts/install.sh --version v0.1.0-alpha` on a fresh runner | **PASS** | same run |
| Released binary: `version`, `doctor` (all PASS), backup + verify on PostgreSQL 16 → VERIFIED (46,800 rows, 10/10 steps) | **PASS** | same run |
| Corrupted artifact with the released binary → FAILED / CHECKSUM_MISMATCH, exit 3, no restore attempted | **PASS** | same run |
| No leftover resources | **PASS** | same run |
| Anonymous install from GitHub (public repo, `raw.githubusercontent.com` + release download) | **PASS** | manual check from the dev sandbox |


## v0.2.0-alpha candidate (2026-11-10, local; CI run #18 green on b5c73ce)

| Area | Result | Test |
|---|---|---|
| Unit tests incl. web API (auth on every endpoint, forged/expired sessions, cookie flags with/without TLS, logout, CSRF header, rate limit 429 + audit, security headers, SPA fallback, listen/token policy, secret redaction in API) | **PASS** | `go test -race ./...` |
| Version-matched client tools (per-major dirs, PATH fallback, mismatched dir rejected, `UNTH_PG_BINDIR`) | **PASS** | `internal/postgres` |
| Panel end-to-end with the real binary: login, Verify now → real isolated restore → VERIFIED, concurrent verify refused (409), audit, token not logged | **PASS** | `TestWebPanelEndToEnd` |
| Panel in Chromium (Playwright): wrong token error, login, overview, backup detail with 4/4 pipeline and report, activity, settings, sign out locks API, no horizontal overflow at 390 px, no console errors | **PASS** | `tests/e2e/panel.mjs` via `UNTH_TEST_BROWSER=1` |
| `unth serve` refuses missing/short token and plain HTTP on 0.0.0.0 | **PASS** | `TestServeRefusesUnsafeConfig` |
| Container image: `doctor`, two `unth run` jobs with retention, volume job — unth inside a container joins/leaves its sandbox networks, no leftovers | **PASS** (local image on `postgres:16` base; the PGDG 13–17 image is built and tested in CI for PostgreSQL 12/16/17) | `TestContainerImageRunsJob` |
| `.deb` in a clean Debian container: install, user/groups, permissions (/, data dir, jobs dir), unit paths, run as `unth`, removal keeps backups | **PASS** | `scripts/test-deb.sh` |
| Installer | **PASS** (6/6) | `make test-install` |
| Full integration suite | **PASS** 34, SKIP 1 (opt-in large DB) | `make test-integration` |
| Web build reproducible (embedded dist == fresh build) | **PASS** | `make web` + `git diff` |
| gosec / staticcheck / gitleaks / npm audit | **PASS** (gosec: 4 reviewed and annotated) | |

Defects found by these tests and fixed: pg_restore 17 → PostgreSQL ≤16 `transaction_timeout`
restore failure (version-matched tools); `.deb` packaged its root directory as 0700; two test
assertion errors.

## Release v0.2.0-alpha — build, publish and clean-machine test (2026-11-10)

Release build: run #3 — https://github.com/unmadethings/unthrecover/actions/runs/42839089977 (commit b5c73ce; tests, govulncheck, binaries, `.deb` amd64/arm64, SHA256SUMS, multi-arch GHCR image).
Clean-machine test: publish-release run #4 — https://github.com/unmadethings/unthrecover/actions/runs/42842743257

| Check | Result |
|---|---|
| All published assets match `SHA256SUMS` (`sha256sum -c`) | **PASS** |
| `scripts/install.sh --version v0.2.0-alpha` on a fresh runner; `version`, `doctor` | **PASS** |
| Released binary: backup + verify on PostgreSQL 16 → VERIFIED; corrupted artifact → exit 3, never VERIFIED | **PASS** |
| `unth run` twice with `keep_last: 1` → both VERIFIED, older one EXPIRED | **PASS** |
| `unth serve`: API 401 without session, token login, overview shows a verified workload, token never in the server log | **PASS** |
| `.deb` installed with `apt` on Ubuntu 24.04: `/usr/bin/unth` version, `unth` user, systemd units, data dir `700 unth` | **PASS** |
| Container image pulled from GHCR (authenticated): `version`, PostgreSQL clients 13–17 present | **PASS**; package made public 2026-11-10, anonymous manifest fetch for `v0.2.0-alpha` → 200 |
| No leftover managed resources | **PASS** |

Defect found by this test and fixed (9ef5336): GitHub rewrites `~` in uploaded asset names
(`unthrecover_0.2.0~alpha_amd64.deb` was published as `unthrecover_0.2.0.alpha_amd64.deb`), so
`SHA256SUMS` listed missing files (publish-release run #3 failed:
https://github.com/unmadethings/unthrecover/actions/runs/42841078024). The `.deb` file name now
uses the tag (`unthrecover_0.2.0-alpha_amd64.deb`; the package version inside stays `0.2.0~alpha`).
For v0.2.0-alpha the publish workflow renamed the two `SHA256SUMS` entries to the asset with the
identical hash; no hash was recomputed. v0.2.0-alpha's `.deb` assets therefore keep the
`0.2.0.alpha` spelling; later releases use the tag spelling.
