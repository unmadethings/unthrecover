# Changelog

All notable changes are documented here. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versioning: [SemVer](https://semver.org/).

## [Unreleased] — targeted as `v0.1.0-alpha` (M0: PostgreSQL recovery proof)

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
- Linux only; tested with PostgreSQL 16 and Docker Engine 29.
- No encryption, no signatures, no remote storage, no scheduling/retention.
- Roles/ownership/privileges are not verified.
- Not production-ready.
