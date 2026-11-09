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
| govulncheck | **FAIL → fixed** | Blocked locally (HTTP 403). First CI run found GO-2026-5004 (pgx 5.7.5), GO-2026-6629 and GO-2026-5970 (x/text 0.27.0); upgraded to pgx 5.12.0 / x/text 0.44.0; re-verification in CI pending |
| GitHub Actions CI | **run #1: FAIL** (govulncheck) | secret scan, lint, unit tests, build passed; integration skipped because the job failed earlier; re-run after fix pending |
| Disk-full test in CI | expected **NOT RUN** on GitHub runners | needs mount privileges; skipped with reason |
| PostgreSQL 12–15 sources | **NOT RUN** | not in matrix yet (M1) |
| macOS / Docker Desktop | **NOT RUN** | unsupported in M0 |

## Defects found and fixed during testing
1. `UpdateBackup` overwrote `verification_status` with an empty string (unit test).
2. `"char"` catalog columns made fingerprint SQL ambiguous under `search_path=pg_catalog` (first E2E run).
3. Sequence values compared for equality — false failure under concurrent writes; now monotonic (integration test).
4. Interrupt test initially signalled during `pg_restore --list`, not the restore; harness fixed to target the real restore.
5. Vulnerable dependencies (pgx SQL-injection placeholder confusion, x/text DoS) found by govulncheck in CI; upgraded (minimum Go is now 1.26).
6. CI run #2: Go 1.26.0 standard-library vulnerabilities (fixed in 1.26.1+); `toolchain go1.26.10` pinned in go.mod.
