# Changelog

All notable changes are documented here. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning: [SemVer](https://semver.org/).

## [Unreleased] — M1 (Self-hosted Community Edition)

### Added
- `unth run`: scheduled job = per-job lock → backup → verify → retention → notify; exit code 4
  when the job succeeded but a notification failed.
- `unth prune` and `retention:` (`keep_last`, `keep_daily`, `keep_weekly`, `max_age`); the newest
  VERIFIED backup and backups under verification are never expired; status `EXPIRED`.
- Notifications: generic webhook (JSON, optional HMAC-SHA256 signature) and Slack-compatible;
  URLs/secrets only from environment variables, https required (loopback http allowed),
  redirects not followed.
- Docker volume backups (`unth backup volume`, `type: docker-volume`): consistency `offline`
  (refuse with running writers) or `stop` (stop/restart exactly the mounting containers,
  audited); archive via never-started helper containers; per-file manifest; verification by
  restore into a new isolated volume and full file comparison; unsafe archives rejected.
- systemd service/timer templates (hardened) and a cron example; docs/SCHEDULING.md.
- CI: PostgreSQL 12–17 integration matrix; disk-full test runs on CI.

## [0.1.0-alpha] — draft (M0: PostgreSQL recovery proof)

### Added
- `unth` CLI: `version`, `doctor`, `discover docker|postgres`, `backup postgres|list|show`,
  `verify`, `report`, `cleanup`, `audit list`; text and JSON output; documented exit codes.
- Snapshot-consistent PostgreSQL logical backups (`pg_export_snapshot` + `pg_dump --snapshot`),
  SHA-256, manifest, private temp files, cleanup on failure, free-space pre-flight.
- Recovery verification engine: real restore into a fresh isolated container (internal
  network, dropped capabilities, resource limits), 10 mandatory steps, data/schema/constraint/
  sequence/assertion comparison, label-scoped cleanup; `VERIFIED` only via `Decide()`.
- JSON/text reports with verification scope and RTO/RPO caveats.
- SQLite metadata with versioned migrations and an audit trail.
- Interrupted-operation recovery (`unth cleanup`).
- Unit tests, Docker integration suite covering the 14 M0 acceptance scenarios, CI and release workflows.

### Known limitations
- Linux only; tested with PostgreSQL 12–17 (CI matrix) and Docker Engine 28/29.
- No encryption, no signatures, no remote storage, no scheduling/retention.
- Roles/ownership/privileges are not verified.
- Not production-ready.
