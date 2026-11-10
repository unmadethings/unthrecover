# CLI reference — `unth`

This page documents the commands as implemented in `v0.1.0-alpha`. Every command is
covered by the CLI smoke/integration tests in `tests/integration`.

## Global flags

| Flag | Default | Description |
|---|---|---|
| `--data-dir` | `$UNTH_DATA_DIR`, else `$XDG_DATA_HOME/unthrecover`, else `~/.local/share/unthrecover` | Metadata DB, backups, reports. Created `0700`. |
| `--log-level` | `warn` | `debug`, `info`, `warn`, `error`. Logs go to stderr and are secret-redacted. |

Environment: `DOCKER_HOST` (only `unix://` sockets are accepted), `UNTH_DATA_DIR`,
`UNTH_PG_BINDIR` (force PostgreSQL client tools from one directory), `UNTH_SANDBOX_JOIN`
(container name/ID or `auto`: attach unth's own container to sandbox networks; set in the image),
`UNTH_UI_TOKEN` (`unth serve`).

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success (`verify`: backup is `VERIFIED`) |
| 1 | Operation failed (backup failed, Docker unreachable, doctor found failures, …) |
| 2 | Usage or configuration error, unknown backup/run ID |
| 3 | `verify` completed but the backup is **not** verified (`FAILED`) |
| 4 | `run`: job succeeded but a notification could not be delivered |
| 5 | `run`/`offsite push`: backup VERIFIED locally but the offsite copy failed or could not be proven; nothing pruned |
| 130 | Cancelled (SIGINT/SIGTERM) |

## Commands

### `unth version [--format text|json]`
Prints version, commit, build date, Go version and platform.

### `unth doctor [--format text|json]`
Checks platform, `pg_dump`/`pg_restore` versions, Docker Engine reachability and API
version, data directory, free space, and unfinished operations. Exit 1 if any check FAILs.

### `unth discover docker [--format text|json]`
Read-only inventory of containers and volumes with a backup-support assessment. Only GET
requests are sent to the Docker Engine.

### `unth discover postgres --config FILE [--format text|json]`
Connects with the job configuration (read-only transaction), reports server version,
database size, table count, recovery state, and whether the local `pg_dump` can back it up.
Exit 1 if incompatible.

### `unth backup postgres --config FILE [--format text|json]`
Creates a snapshot-consistent logical backup:
pre-flight (tools, free space ≥ `backup.min_free_bytes`) → `REPEATABLE READ` transaction →
`pg_export_snapshot()` → source fingerprint → `pg_dump --format=custom --snapshot=…`
streamed to a private temp file while hashing → `pg_restore --list` → move into
`backups/<id>/` with `manifest.json`. Status ends as `BACKUP_CREATED`, `FAILED` or `CANCELLED`.
Partial files are always removed on failure.

### `unth backup volume --config FILE [--format text|json]`
File-level backup of a Docker volume (`source.type: docker-volume`). `consistency: offline`
refuses while a running container mounts the volume (`CONSISTENCY_UNSAFE`); `consistency: stop`
stops exactly those containers for the copy and restarts them. See
[DOCKER_VOLUME_BACKUP.md](DOCKER_VOLUME_BACKUP.md). `backup postgres` and `backup volume` refuse
configs of the other type (exit 2); `unth run` handles both.

### `unth backup list [--format text|json] [--limit N]`
Newest first, with backup status and latest verification status.

### `unth backup show <backup-id> [--format text|json]`
Backup details and all verification runs.

### `unth verify <backup-id> [--format text|json] [--timeout DUR] [--image REF] [--pull if-missing|always|never]`
Runs a **new** verification run (new run ID) with a real isolated restore. Settings come
from the configuration stored with the backup; flags override them for this run.
Mandatory steps: `artifact_accessible`, `manifest_valid`, `checksum_sha256`,
`archive_readable`, `isolated_environment`, `restore`, `postgres_health`,
`data_integrity`, `schema_and_constraints`, `assertion_queries`.
All created Docker resources are removed at the end, whatever the outcome.
Only backups in status `BACKUP_CREATED` can be verified.

### `unth report <backup-id> [--run RUN_ID] [--format text|json]`
Shows the stored report of the latest (or given) verification run.

### `unth cleanup [--dry-run] [--format text|json]`
Interrupted-operation recovery. Marks backups/runs whose owning process is gone as
`FAILED` (`INTERRUPTED`), deletes their partial files, and removes Docker resources labeled
`io.unthrecover.managed=true` that belong to inactive runs. Never touches unlabeled resources.
Idempotent.

### `unth run --config FILE [--format text|json]`
The command to schedule: per-job lock → backup → verify → prune (only if verified) →
notify. Exit codes 0/1/3/4/130 as above. See [SCHEDULING.md](SCHEDULING.md).

### `unth prune --config FILE [--dry-run] [--format text|json]`
Applies the job's `retention:` rules to its workload. Expired backups lose their artifact
directory and become `EXPIRED`; history is kept. The newest VERIFIED backup is never expired.
Exit 2 if no retention rules are configured.

### `unth serve [--listen 127.0.0.1:8484] [--token-file FILE] [--tls-cert C --tls-key K] [--behind-proxy]`
Authenticated web management panel and JSON API. See [WEB_PANEL.md](WEB_PANEL.md). The admin
token comes from `--token-file` (0600) or `UNTH_UI_TOKEN` (min. 24 chars); plain HTTP only on
loopback, otherwise TLS is required — or `--behind-proxy`, which declares that a reverse proxy
terminates TLS (Coolify, Dokploy, Traefik, Caddy, the Compose file). Never publish such a port
directly to a network.

### `unth schedule (--at HH:MM | --every DUR) [--jobs-dir /etc/unth/jobs] [--run-at-start]`
Scheduler for containers without systemd/cron: runs every `*.yaml` job in `--jobs-dir`, one after
another, each exactly like `unth run`. All job files are validated at start (exit 2 on errors).
A failing job is logged (one line per job: outcome, verification status, error category) and
does not stop the others. `--at` uses `TZ` (default UTC in the image); `--every` is at least 1m.
The job list is re-read every round. SIGINT/SIGTERM stops it; a running job is recorded as
CANCELLED.

### `unth keys generate [--dir /etc/unth/keys]`
Creates `age.key` (encryption identity, 0600), `sign.key` (Ed25519, 0600) and `sign.pub`.
Never overwrites existing key files. See [OFFSITE.md](OFFSITE.md).

### `unth offsite list|check|push|fetch|prune --config FILE`
Offsite copies of the job's workload ([OFFSITE.md](OFFSITE.md)): `list` complete copies on the
target; `check <id>` downloads a copy again and proves signature, hashes and decryption (exit 3
if not OK); `push <id>` copies an existing VERIFIED backup (exit 5 on failure); `fetch <id>`
disaster recovery — downloads, proves and imports a copy as a NOT_VERIFIED local backup (then
run `unth verify`); `prune [--dry-run]` applies `offsite.retention`.

### `unth audit list [--format text|json] [--limit N]`
Audit trail of backups, verifications and cleanups (no secret values).

## Configuration file

See [`examples/backup.yaml`](../examples/backup.yaml). Unknown keys are rejected. A
literal `password` key is rejected: use `password_env` or `password_file` (must be `0600`).

| Key | Default | Notes |
|---|---|---|
| `version` | `1` | |
| `source.name` | — | Workload ID (`[A-Za-z0-9][A-Za-z0-9_.-]{0,62}`) |
| `source.type` | `postgres` | Only `postgres` |
| `source.host` / `port` | — / `5432` | |
| `source.database` / `user` | — | |
| `source.password_env` / `password_file` | — | At most one; none = trust/peer/`~/.pgpass` |
| `source.sslmode` | `prefer` | libpq values |
| `source.connect_timeout` | `10s` | |
| `backup.timeout` | `1h` | |
| `backup.min_free_bytes` | `268435456` | Pre-flight free-space requirement |
| `verification.restore_image` | `postgres:<source major>` | |
| `verification.image_pull` | `if-missing` | `if-missing`, `always`, `never` |
| `verification.timeout` | `30m` | Whole verification run |
| `verification.memory_mb` / `cpus` / `pids_limit` | `1024` / `1.0` / `512` | Sandbox limits |
| `verification.table_hash_max_rows` | `100000` | Tables up to this size get a full content hash |
| `verification.queries[]` | — | `{name, sql}` read-only assertions |
| `source.type: docker-volume` → `volume` | — | Docker volume name |
| `source.consistency` | `offline` | `offline` or `stop` |
| `source.helper_image` / `stop_timeout` | `busybox:1.36.1` / `30s` | Never-started mount container image; graceful stop timeout |
| `retention.keep_last` / `keep_daily` / `keep_weekly` | — | See [SCHEDULING.md](SCHEDULING.md) |
| `retention.max_age` | — | e.g. `2160h`; minimum `1h` |
| `notifications[].name` / `type` | — | `type`: `webhook` or `slack` |
| `notifications[].url_env` | — | Env var holding the URL (required; URLs are secrets) |
| `notifications[].secret_env` | — | Optional HMAC-SHA256 key (webhook only) |
| `notifications[].on` / `timeout` | `failure` / `10s` | `failure` or `always`; 1s–1m |
