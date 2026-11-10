# PostgreSQL backup

## What is backed up
One database per job, as a `pg_dump` custom-format archive (compression level 6): schemas,
tables and data, indexes, constraints, sequences, views, materialized view definitions,
functions, triggers, types, extensions (as `CREATE EXTENSION`), comments, large objects.

**Not included** (a property of `pg_dump`, not a bug): cluster roles, tablespaces,
`postgresql.conf`/`pg_hba.conf`, other databases. Use `pg_dumpall --globals-only` separately
if you need roles. Logical backups restore to the snapshot moment — no point-in-time recovery.

## Version compatibility
- Source server ≥ 12 (`MinServerMajor`).
- Local `pg_dump` major must be ≥ source major; otherwise the backup fails up-front with
  `UNSUPPORTED_VERSION` (tested with real servers: PostgreSQL 17 vs pg_dump 16, 18 vs pg_dump 17).
- Tested in CI with source and restore on the same major: PostgreSQL 12, 13, 14, 15 and 16
  (pg_dump 16) and 17 (pg_dump 17); full integration suite per version.
- Restore image defaults to `postgres:<source major>`.
- **Version-matched client tools.** unth prefers `pg_dump`/`pg_restore` whose major equals the
  source server's, looking in `/usr/lib/postgresql/<N>/bin` (Debian/Ubuntu), `/usr/libexec/postgresql<N>`
  (Alpine), `/usr/pgsql-<N>/bin` (RHEL/PGDG) and Homebrew; without an exact match it takes the
  nearest newer installed major (e.g. 13 for a PostgreSQL 12 server), then `PATH`. The archive is restored with
  the `pg_restore` matching the `pg_dump` that wrote it. This matters: `pg_restore` 17 emits
  `SET transaction_timeout`, which PostgreSQL 16 and older reject (found by the container-image
  test; such a backup is correctly reported FAILED/RESTORE_FAILED with a hint). Install the
  client for each source major you back up (e.g. `postgresql-client-16`), or force a directory
  with `UNTH_PG_BINDIR=/path/to/bin`. `unth doctor` lists the installed majors.

## Recommended backup role
```sql
CREATE ROLE unth_backup LOGIN PASSWORD '…';
GRANT pg_read_all_data TO unth_backup;   -- PostgreSQL 14+
```
For older versions grant `SELECT` on all tables/sequences and `USAGE` on schemas.
`pg_export_snapshot()` needs no extra privilege.

## Consistency
Fingerprint and dump share one exported snapshot, so concurrent writes during the backup do
not cause false verification failures (integration-tested). `pg_dump` takes `ACCESS SHARE`
locks; `--lock-wait-timeout=60s` prevents waiting forever behind DDL.

## Credentials
`password_env` (variable name) or `password_file` (`0600`). The password is written to a
private temporary `PGPASSFILE` for the duration of the tool call and removed afterwards.
With neither set, libpq falls back to trust/peer auth or your `~/.pgpass`.

## Failure behaviour
Any failure removes partial files and records `FAILED` with a category
(`AUTHENTICATION_FAILED`, `CONNECTION_FAILED`, `UNSUPPORTED_VERSION`, `INSUFFICIENT_STORAGE`,
`BACKUP_FAILED`, `TIMEOUT`, …). SIGINT records `CANCELLED`.
