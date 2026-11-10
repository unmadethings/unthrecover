# Unth Recover

> **Your backup is not your recovery.** Don't just back up — prove you can recover.

Unth Recover is an open-source disaster-recovery tool by **Unmade Things** that backs up
PostgreSQL databases and **proves** each backup can be recovered: it restores the backup
into a brand-new, isolated PostgreSQL container and checks the data, schema and
constraints against a fingerprint taken from the source *inside the same snapshot as
the dump*. Only then is a backup marked `VERIFIED`.

[Türkçe README](README.tr.md) · License: [Apache-2.0](LICENSE)

> **Status: `v0.1.0-alpha` (M0 — PostgreSQL recovery proof).** Pre-release software.
> It is **not production-ready**; see [Known limitations](#known-limitations).

---

## The problem

Most backup tools tell you a backup *was created*. A file existing — even with a valid
checksum — says nothing about whether it can be restored: the dump may be truncated,
an extension may be missing in the target, a constraint may fail on load, or the dump
may simply not contain what you think it contains. Teams usually find out during an
incident. Even platform docs recommend restoring into a disposable database to check
(e.g. [Coolify's backup docs](https://coolify.io/docs/databases/backups)) — but leave it
as a manual task.

## What makes Unth Recover different

| | Typical backup tool | Unth Recover |
|---|---|---|
| Creates a backup | ✅ | ✅ (`pg_dump`, custom format, snapshot-consistent) |
| Checksum / repository check | ✅ | ✅ SHA-256 vs. manifest **and** separate metadata DB |
| **Restores every verification into a fresh isolated container** | manual | ✅ automatic, internal network, no published ports, resource limits |
| **Compares restored data with the source snapshot** | ❌ | ✅ exact row counts, per-table content hashes, schema objects, constraints, sequences, custom assertions |
| Honest report of what was *not* verified | ❌ | ✅ explicit `verification_scope` |
| Separates observed test restore time from RTO / RPO | ❌ | ✅ |

The core rule is enforced in code and tests: **a backup can only become `VERIFIED` after a
real restore in the current run and every mandatory check passing. A checksum alone never
verifies a backup.**

Unth Recover is complementary to tools like restic, Kopia, BorgBackup or pgBackRest — see
[docs/research/COMPETITIVE_ANALYSIS.md](docs/research/COMPETITIVE_ANALYSIS.md).

## How verification works

```
unth backup postgres            unth verify <backup-id>
────────────────────            ───────────────────────────────────────────────────────────
BEGIN REPEATABLE READ           1 artifact accessible  (path-safe, no symlinks, size)
pg_export_snapshot()   ─┐       2 manifest valid       (matches metadata DB)
fingerprint (counts,    │       3 SHA-256 checksum     (recomputed from disk)
  hashes, schema)       │ same  4 archive readable     (pg_restore --list)
pg_dump --snapshot=…  ──┘ snap  5 isolated environment (new container, internal network)
SHA-256, manifest               6 restore              (pg_restore --single-transaction --exit-on-error)
→ BACKUP_CREATED                7 PostgreSQL health
                                8 data integrity       (row counts + content hashes)
                                9 schema & constraints (+ sequence monotonicity)
                               10 assertion queries    (your own SQL)
                                → VERIFIED only if 1–10 all PASS · resources always removed
```

## Installation

Requirements: Linux, Docker Engine (socket access), PostgreSQL client tools
(`pg_dump`, `pg_restore`) whose major version is ≥ your server's. Go 1.26+ to build.

```bash
git clone https://github.com/unmadethings/unthrecover.git
```

```bash
cd unthrecover && make build && sudo install bin/unth /usr/local/bin/unth
```

```bash
unth doctor
```

Details: [docs/INSTALLATION.md](docs/INSTALLATION.md).

## Quick start

```bash
export SHOP_DB_PASSWORD='…'          # never put passwords in the config file
unth discover postgres --config examples/backup.yaml
unth backup postgres --config examples/backup.yaml
unth backup list
unth verify <backup-id>              # exit 0 = VERIFIED, 3 = not verified
unth report <backup-id> --format json
unth run --config examples/backup.yaml   # what you schedule: backup + verify + prune + notify
```

Scheduling, retention and notifications: [docs/SCHEDULING.md](docs/SCHEDULING.md).
Docker volumes: [docs/DOCKER_VOLUME_BACKUP.md](docs/DOCKER_VOLUME_BACKUP.md) (`unth backup volume`).

Walkthrough with a disposable demo database: [docs/QUICKSTART.md](docs/QUICKSTART.md).
All commands: [docs/CLI_REFERENCE.md](docs/CLI_REFERENCE.md).

## What is supported (M0)

- PostgreSQL 12 – (your `pg_dump` major) logical backups; **tested in CI with PostgreSQL 12, 13, 14, 15, 16 and 17**
- Local repository (`--data-dir`, default `~/.local/share/unthrecover`)
- SQLite metadata with versioned migrations; audit trail (`unth audit list`)
- Isolated restore testing in Docker; JSON and text reports
- Read-only discovery of Docker containers/volumes and PostgreSQL
- Interrupted-operation recovery (`unth cleanup`)
- Docker volume backups with consistency policies (`offline` / `stop`) and per-file verification in an isolated volume
- Scheduled jobs: `unth run` (backup → verify → prune → notify) with systemd/cron templates
- Retention policies (`unth prune`; the newest VERIFIED backup is never expired)
- Webhook (HMAC-signed) and Slack notifications

## Not implemented yet

S3-compatible storage, encryption, signed manifests, web UI,
production restore, multi-server. See [docs/MASTER_PLAN.md](docs/MASTER_PLAN.md).
Nothing in this list is partially present behind a flag.

## Known limitations

- Linux only (the host must reach Docker bridge networks).
- Artifacts are **not encrypted** and checksums are **not signatures** — protect the data directory. See [docs/BACKUP_SECURITY.md](docs/BACKUP_SECURITY.md).
- Roles, ownership, privileges and cluster settings are not part of a single-database dump and are not verified.
- Docker socket access is root-equivalent. See [docs/SECURITY_THREAT_MODEL.md](docs/SECURITY_THREAT_MODEL.md).

## Documentation

[Architecture](docs/ARCHITECTURE.md) · [Technical decisions](docs/TECHNICAL_DECISIONS.md) ·
[Recovery verification](docs/RECOVERY_VERIFICATION.md) · [PostgreSQL backup](docs/POSTGRESQL_BACKUP.md) ·
[Restore safety](docs/RESTORE_SAFETY.md) · [Threat model](docs/SECURITY_THREAT_MODEL.md) ·
[Troubleshooting](docs/TROUBLESHOOTING.md) · [Test results](docs/TEST_RESULTS.md) ·
[Release criteria](docs/RELEASE_CRITERIA.md) · [Product vision](docs/PRODUCT_VISION.md)

## Contributing

Contributions are welcome — please read [CONTRIBUTING.md](CONTRIBUTING.md) and the
[Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately per [SECURITY.md](SECURITY.md).

## Sponsoring

GitHub Sponsors is not yet enabled for Unmade Things. Until it is, the best way to support
the project is to try it, report issues, and tell us how you test recoveries today.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
