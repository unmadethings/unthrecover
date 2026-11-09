# Recovery verification

`unth verify <backup-id>` performs a complete recovery test **every time**. It never reuses a
previous result: each invocation has its own run ID, sandbox and report.

## Steps (all mandatory)

| # | Step | Passes when | Failure category |
|---|---|---|---|
| 1 | `artifact_accessible` | path inside data dir, no symlinks, regular file, size = recorded | `ARTIFACT_MISSING`, `ARTIFACT_INVALID`, `CHECKSUM_MISMATCH` |
| 2 | `manifest_valid` | manifest parses, belongs to this backup, checksum/size/fingerprint digest match the metadata DB | `MANIFEST_INVALID`, `CHECKSUM_MISMATCH` |
| 3 | `checksum_sha256` | SHA-256 recomputed from disk equals the recorded value | `CHECKSUM_MISMATCH` |
| 4 | `archive_readable` | `pg_restore --list` succeeds with ≥1 TOC entry | `ARTIFACT_INVALID` |
| 5 | `isolated_environment` | sandbox created and PostgreSQL answers `SELECT 1` | `DOCKER_UNAVAILABLE`, `ENVIRONMENT_FAILED` |
| 6 | `restore` | `pg_restore --single-transaction --exit-on-error --no-owner --no-privileges` exits 0 | `RESTORE_FAILED` |
| 7 | `postgres_health` | restored DB accepts connections and can be fingerprinted | `RESTORE_FAILED` |
| 8 | `data_integrity` | every table present, exact row counts equal, content hashes equal (tables ≤ `table_hash_max_rows`), no unexpected tables, sequences not lower than captured | `DATA_MISMATCH` |
| 9 | `schema_and_constraints` | identical sets of schemas, tables/views, columns (type, nullability), constraints (with definitions), indexes (definitions), sequences, functions, triggers, enum/domain/composite types, extensions | `SCHEMA_MISMATCH` |
| 10 | `assertion_queries` | every configured query returns the same rows (count + hash) | `ASSERTION_FAILED` |

After step 4 the run is `INTEGRITY_CHECKED`; during 5–10 it is `RESTORE_TESTING`. Once a step
fails, later steps are recorded as `NOT_RUN`. A timeout yields `TIMEOUT`; SIGINT yields
`CANCELLED`.

**`VERIFIED` = all ten steps PASS in this run.** Implemented in `internal/verify/decide.go`
and covered by unit tests that prove checksum-only, partial, tampered-flag and
conflicting-duplicate runs can never be verified.

If no assertion queries are configured, step 10 passes but the report lists
"application-specific assertions" under **not covered** — Unth Recover never claims
application-level correctness it did not test.

## How the expected state is captured
Inside the backup's `REPEATABLE READ` transaction, after `pg_export_snapshot()` and before
`pg_dump --snapshot`. Row text is hashed in `COLLATE "C"` order with fixed session settings
(`DateStyle`, `TimeZone`, `extra_float_digits`, `bytea_output`, `search_path=pg_catalog`)
so that source and restored outputs are byte-comparable.

## Report
JSON (`--format json`) and text. Contains backup ID, source, size, checksum result, restore
result, scope (covered / not covered), PostgreSQL and container health, per-step timings and
details, comparison with mismatches, cleanup result, and recovery objectives:

- `observed_test_restore_duration_ms` — **not an RTO**: real recovery adds detection,
  provisioning, transfer, cut-over and human response time.
- `estimated_data_loss_window_seconds_at_report_time` — time since the snapshot; an
  **estimate of exposure, not a verified RPO**.

Reports are stored in the metadata DB and `reports/<run-id>.json`; `unth report` reads them.
