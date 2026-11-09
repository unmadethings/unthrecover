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
  `UNSUPPORTED_VERSION` (tested with a real PostgreSQL 17 server and pg_dump 16).
- Tested: PostgreSQL 16.15 (source and restore). Other versions in range are expected to work
  but are not yet in the CI matrix.
- Restore image defaults to `postgres:<source major>`.

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
