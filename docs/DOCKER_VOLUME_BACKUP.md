# Docker volume backup

File-level backups of Docker volumes (uploads, media, configuration, application state),
with the same rule as databases: **a backup is VERIFIED only after it was really restored
into a new isolated volume and every file matched**.

```yaml
version: 1
source:
  name: app-uploads
  type: docker-volume
  volume: myapp_uploads
  consistency: offline        # offline (default) | stop
  helper_image: busybox:1.36.1
  stop_timeout: 30s           # consistency: stop only
verification:
  image_pull: if-missing
retention:
  keep_daily: 14
```

```bash
unth backup volume --config uploads.yaml
```

```bash
unth verify <backup-id>
```

```bash
unth run --config uploads.yaml
```

## Consistency policies

Copying files while a process writes them can capture a half-written state. Unth Recover never
claims such a copy is consistent.

| Policy | Behaviour |
|---|---|
| `offline` (default) | If **any running** container mounts the volume, the backup is refused with `CONSISTENCY_UNSAFE`. Nothing is touched. |
| `stop` | Exactly the running containers that mount the volume are stopped (graceful, `stop_timeout`), the archive is taken, and those containers are started again — also when the backup fails. Containers that were not running stay stopped. Every stop/start is recorded in the audit trail. Downtime = archive time. |

If a mounting container runs a database image (PostgreSQL, MySQL/MariaDB, MongoDB, Redis, …)
a warning is stored in the manifest: a stopped database's files are a valid *cold* backup, but a
logical backup (`type: postgres`) is preferred where supported. If restarting a container fails,
the backup is kept, the failure is audited and the command exits 1.

## How it works

- **Backup:** a helper container is *created but never started* (no process, no network, all
  capabilities dropped) with the volume mounted read-only at `/data`. The Docker archive API
  streams a tar of `/data`; unth writes it to `<data-dir>/backups/<id>/volume.tar` while
  computing its SHA-256 and a per-entry manifest (path, type, mode, uid/gid, size, SHA-256,
  symlink target). The helper is removed afterwards.
- **Verification:** artifact/manifest/checksum checks → `archive_readable` (the tar is parsed;
  absolute paths, `..`, entries outside the volume root, hard links pointing outside, duplicates,
  unsupported types and size/entry-count overruns are rejected, and the content must match the
  manifest) → `isolated_environment` (new labeled volume + never-started helper) → `restore`
  (the Docker daemon extracts the archive into the new volume) → `file_integrity` (the restored
  volume is archived again and every entry compared). Resources are removed by run label.
- unth **never extracts a backup onto the host filesystem**.

Mandatory steps for VERIFIED: `artifact_accessible`, `manifest_valid`, `checksum_sha256`,
`archive_readable`, `isolated_environment`, `restore`, `file_integrity`.

## Not covered (stated in every report)

Application-level consistency beyond the chosen policy; application health (no app container is
started from the restored volume); extended attributes, ACLs, hard-link identity and modification
times; artifact authenticity (checksums are not signatures).

## Requirements

The helper image must be available locally or pullable (`verification.image_pull`); any image
works because its process is never run. Restoring into an *existing* volume is not implemented.
