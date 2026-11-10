# Changelog

All notable changes are documented here. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning: [SemVer](https://semver.org/).

## [0.2.0-alpha] — 2026-11-10 — M1 (Self-hosted Community Edition) + M2 web panel preview

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
- Web management panel (M2): `unth serve` with token login, HttpOnly/SameSite=Strict sessions,
  CSRF header, login rate limiting, strict CSP; TLS required for non-loopback listening. Views:
  overview (workloads, last verified recovery, 30-day success rate, storage, avg. test restore),
  backups with the CREATED → INTEGRITY CHECKED → RESTORE TESTED → VERIFIED pipeline, run reports
  with failure reason and suggested fix, "Verify now", activity log, settings. React + TypeScript +
  Vite, embedded in the binary.
- Packaging: `.deb` (systemd units, `unth` user), container image built from release binaries
  (unth attaches itself to sandbox networks: `UNTH_SANDBOX_JOIN`).
- Version-matched PostgreSQL client tools: unth uses `pg_dump`/`pg_restore` of the source
  server's major from standard per-version directories (override: `UNTH_PG_BINDIR`). Fixes
  restores failing with `unrecognized configuration parameter "transaction_timeout"` when
  client tools 17 were used for PostgreSQL ≤ 16 sources (found by the new container test).
- User interview kit (docs/research/KULLANICI_GORUSMELERI.md).
- `scripts/install.sh`: downloads a release binary + SHA256SUMS, verifies, smoke-runs and
  installs atomically; refuses mismatches, missing entries, invalid versions and plain http.
- CI: PostgreSQL 12–17 integration matrix; disk-full test runs on CI; installer tests.

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
