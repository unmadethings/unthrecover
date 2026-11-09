# Architecture

Unth Recover M0 is a single Go binary (`unth`) with a modular internal layout. There is no
server process, no network listener and no external service dependency.

```
cmd/unth                 entrypoint; SIGINT/SIGTERM → context cancellation
internal/cli             cobra commands, exit codes, secret-redacting slog handler
internal/config          strict YAML config (unknown keys rejected, no plaintext passwords)
internal/discovery       Module A — read-only Docker / PostgreSQL inventory
internal/backup          Module B — snapshot-consistent pg_dump backup
internal/restore         Module C — isolated sandbox lifecycle + label-scoped cleanup
internal/verify          Module D — verification engine; Decide() = the VERIFIED rule
internal/report          Module E — report model, JSON/text rendering
internal/postgres        pg_dump/pg_restore wrappers, fingerprint capture + comparison
internal/docker          minimal Docker Engine API client (unix socket)
internal/manifest        manifest.json + SHA-256
internal/metadata        SQLite store, versioned migrations, audit events
internal/recovery        interrupted-operation recovery (`unth cleanup`)
internal/repo            data-directory layout, safe path resolution
internal/security        redaction, safe paths, pgpass files, IDs
internal/failure         categorized errors + remediation hints
```

## Data directory

```
<data-dir>/              0700
  metadata.db            SQLite (WAL, synchronous=FULL, foreign keys), 0600
  backups/<backup-id>/   artifact.dump (pg_dump custom format), manifest.json — 0600 files
  reports/<run-id>.json  verification reports
  tmp/<id>.partial       in-progress artifacts (removed on failure / by cleanup)
```

## State model

Backup status: `PENDING → RUNNING → BACKUP_CREATED | FAILED | CANCELLED` (`EXPIRED` is
reserved for retention, not used in M0).

Verification run status: `RUNNING → INTEGRITY_CHECKED → RESTORE_TESTING → VERIFIED | FAILED | CANCELLED`.
The backup row mirrors its latest run in `verification_status` (`NOT_VERIFIED` until the
first run). A later failed run downgrades a previously verified backup.

`VERIFIED` is computed only by `verify.Decide()`, a pure function: every mandatory step must
be present, flagged mandatory, and `PASS`; any `FAIL` anywhere yields `FAILED`.

## Backup data flow

1. Strict config load; password resolved from env/file and registered with the redactor.
2. Pre-flight: tool lookup, free space, `pg_dump` major ≥ server major.
3. `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`; `pg_export_snapshot()`.
4. Fingerprint in that transaction: per-table exact row count and `md5` of rows ordered by
   their text form (`COLLATE "C"`), schema object signatures (tables, columns, constraints,
   indexes, sequences, functions, triggers, types, schemas, extensions), sequence values,
   and user assertion queries.
5. `pg_dump --snapshot=<id>` (connection via `PG*` env vars + private `PGPASSFILE`) streamed
   into `tmp/<id>.partial` (`O_EXCL`, `0600`) through a SHA-256 hasher; `fsync`.
6. Commit, `pg_restore --list`, rename into `backups/<id>/`, write manifest atomically, fsync dir.

## Verification data flow

See [RECOVERY_VERIFICATION.md](RECOVERY_VERIFICATION.md). The sandbox is:
internal bridge network (no egress, ICC disabled), labeled named volume, container with
`CapDrop=ALL` + 5 capabilities needed by the postgres entrypoint, `no-new-privileges`,
memory/CPU/PIDs limits, no published ports, no host mounts, random throw-away password.
`pg_restore` runs on the host against the container IP; the artifact never enters the container.

## Why these choices

See [TECHNICAL_DECISIONS.md](TECHNICAL_DECISIONS.md).
