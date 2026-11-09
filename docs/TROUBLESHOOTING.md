# Troubleshooting

Every failure has a category (shown as `Failure category:` / `error_category`) and, where
possible, a `Suggested fix:` line.

| Category | Typical cause | Fix |
|---|---|---|
| `CONFIG_INVALID` (exit 2) | Unknown key, plaintext `password`, invalid value | Fix the reported key; use `password_env`/`password_file` |
| `AUTHENTICATION_FAILED` | Wrong user/password, `password_file` not `0600` | Check the secret source; `chmod 600` the file |
| `CONNECTION_FAILED` | Host/port wrong, firewall, DB missing | `unth discover postgres --config …` |
| `UNSUPPORTED_VERSION` | `pg_dump` older than server, server < 12 | Install `postgresql-client-<server major>` |
| `TOOL_MISSING` | `pg_dump`/`pg_restore` not on PATH | Install PostgreSQL client tools |
| `INSUFFICIENT_STORAGE` | Free space below `min_free_bytes` or disk filled during dump | Free space or move `--data-dir` |
| `ARTIFACT_MISSING` | Artifact/manifest deleted or moved | Take a new backup |
| `CHECKSUM_MISMATCH` | Artifact or manifest modified/corrupted | Do not trust this backup; investigate storage |
| `ARTIFACT_INVALID` | Truncated/garbled dump, symlink in path | Take a new backup; check storage |
| `MANIFEST_INVALID` | Manifest edited or from another backup | Take a new backup |
| `DOCKER_UNAVAILABLE` | Daemon down, wrong `DOCKER_HOST`, no socket permission | Start Docker / fix permissions; `unth doctor` |
| `ENVIRONMENT_FAILED` | Image missing with `image_pull: never`, registry rate limit, container exits (OOM) | Pull the image, raise `memory_mb` |
| `RESTORE_FAILED` | Missing extension in restore image, incompatible objects | Use an image with the extension (`restore_image`) |
| `DATA_MISMATCH` / `SCHEMA_MISMATCH` | Restored DB differs from the snapshot | Treat the backup as unverified; report a bug if reproducible |
| `ASSERTION_FAILED` | A query failed or returned different rows | Check the query (no trailing `;`, deterministic) |
| `TIMEOUT` | Run exceeded `verification.timeout` | Increase the timeout or `--timeout` |
| `INTERRUPTED` | Process killed; marked by `unth cleanup` | Re-run the operation |

## Leftover containers after a crash
```bash
unth cleanup --dry-run
```

```bash
unth cleanup
```

Only resources labeled `io.unthrecover.managed=true` for inactive runs are removed.

## Docker Hub rate limits
Verification pulls `postgres:<major>` if missing. Pre-pull it, or point `restore_image` to a
mirror, and set `image_pull: never` for fully offline operation.

## macOS / Docker Desktop
Not supported in M0: the host cannot reach container IPs on Docker Desktop.
