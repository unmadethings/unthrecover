# Scheduling, retention and notifications

`unth run --config job.yaml` is the command to schedule. Each run:

1. takes a per-job lock (`<data-dir>/locks/<workload>.lock`); an overlapping run exits 1 without doing anything,
2. creates a backup,
3. verifies it with a real isolated restore (always — there is no "skip verify"),
4. with `offsite:` — copies the verified backup offsite, encrypted, and proves the copy by downloading it again ([OFFSITE.md](OFFSITE.md)),
5. applies the retention policy (offsite, then local) — **only if verification and the offsite copy succeeded**,
6. sends notifications — always attempted, also after failures and cancellations.

| Exit code | Meaning | Scheduler should |
|---|---|---|
| 0 | backup VERIFIED (and pruned, notified) | nothing |
| 1 | backup or prune failed, or another run holds the lock | alert |
| 2 | configuration error | alert |
| 3 | backup created but NOT verified | alert — this backup cannot be trusted |
| 4 | job succeeded but a notification could not be delivered | alert (you would otherwise miss future failures) |
| 5 | backup VERIFIED locally, but the offsite copy failed or could not be proven; nothing pruned | alert — the server is the only place this backup exists |
| 130 | cancelled (SIGINT/SIGTERM) | investigate |

## Containers: `unth schedule` and Docker Compose
Without systemd or cron (Docker Compose, Coolify, Dokploy) run the built-in scheduler:
`unth schedule --at 03:00` runs every job in `/etc/unth/jobs` daily at 03:00, each exactly like
`unth run`. The release ships a ready `compose.yaml` (scheduler + panel); see
[INSTALLATION.md](INSTALLATION.md#docker-compose).

## systemd (recommended on hosts)

Templates: [`deployments/systemd/unth@.service`](../deployments/systemd/unth@.service) and
[`unth@.timer`](../deployments/systemd/unth@.timer). One instance per job.

```bash
sudo useradd --system --home /var/lib/unthrecover --shell /usr/sbin/nologin --groups docker unth
```

```bash
sudo install -m 0644 deployments/systemd/unth@.service deployments/systemd/unth@.timer /etc/systemd/system/
```

```bash
sudo install -d -m 0750 -o root -g unth /etc/unth/jobs
```

```bash
sudo install -m 0640 -o root -g unth examples/backup.yaml /etc/unth/jobs/shop-db.yaml
```

Secrets go into `/etc/unth/jobs/shop-db.env` (`0600 root:root`; systemd reads it before
dropping privileges), e.g. `SHOP_DB_PASSWORD=…`, `UNTH_OPS_WEBHOOK=…`.

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now unth@shop-db.timer
```

```bash
sudo systemctl start unth@shop-db.service && journalctl -u unth@shop-db.service -n 50
```

The service runs hardened (`ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`, no
capabilities, only `/var/lib/unthrecover` writable). The timer runs daily between 01:00 and
03:00 (`RandomizedDelaySec`), and `Persistent=true` catches up after downtime. Exit codes 3 and
4 make the unit fail, so `systemctl --failed` and `OnFailure=` hooks see unverified backups.
Unit syntax was checked with `systemd-analyze verify` (systemd 255).

## cron

See [`deployments/cron/unthrecover.cron`](../deployments/cron/unthrecover.cron). Load the secrets
file with `set -a; . file; set +a` and redirect output to a log; monitor exit codes.

## Retention (`retention:`)

```yaml
retention:
  keep_last: 7      # the 7 newest backups
  keep_daily: 14    # newest backup of each of the 14 most recent UTC days with backups
  keep_weekly: 8    # newest backup of each of the 8 most recent ISO weeks with backups
  max_age: 2160h    # alone: keep backups younger than 90 days; with keep_*: expire anything older
```

A backup is kept if **any** keep rule matches. Regardless of rules, the **newest VERIFIED
backup is never expired**, nor is a backup whose verification is running. Only
`BACKUP_CREATED` backups are considered. Expiring deletes the artifact directory and sets
status `EXPIRED`; metadata, verification runs, reports and audit events are kept. Expired
backups cannot be verified. Preview with:

```bash
unth prune --config job.yaml --dry-run
```

## Notifications (`notifications:`)

```yaml
notifications:
  - name: ops
    type: webhook            # JSON event (below)
    url_env: UNTH_OPS_WEBHOOK
    secret_env: UNTH_OPS_WEBHOOK_SECRET   # optional HMAC-SHA256 signing
    on: always               # failure (default) | always
    timeout: 10s
  - name: chat
    type: slack              # Slack-compatible {"text": "..."} incoming webhook
    url_env: UNTH_SLACK_WEBHOOK
    on: failure
```

- URLs are secrets (Slack URLs contain a token): they are read **only** from environment
  variables, registered for redaction, and never logged; error messages show the host only.
- `https` is required; plain `http` is accepted only for loopback hosts (local relays).
  Redirects are not followed. URLs with `user:pass@` are rejected.
- Delivery failures never change the job result but make `unth run` exit 4.

Generic webhook body (`Content-Type: application/json`, header `X-Unth-Event: unth.run.completed`):

```json
{"event":"unth.run.completed","schema_version":1,"tool_version":"0.1.0-alpha","host":"db1",
 "workload":"shop-db","outcome":"failure","backup_id":"bk-…","backup_status":"BACKUP_CREATED",
 "verification_run_id":"vr-…","verification_status":"FAILED","error_category":"RESTORE_FAILED",
 "error_message":"…","hint":"…","pruned_backup_ids":[],"started_at":"…","finished_at":"…","duration_ms":41234}
```

Signature (when `secret_env` is set): `X-Unth-Timestamp: <unix seconds>` and
`X-Unth-Signature: sha256=<hex HMAC-SHA256(secret, timestamp + "." + raw body)>`. Receivers
should recompute it with a constant-time comparison and reject timestamps older than a few minutes.
