# Master plan

Status legend: ✅ done and tested · 🟡 partial · ⬜ not started. Updated with each milestone.

## M0 — Core recovery proof (current)

| Work package | Acceptance criterion | Status |
|---|---|---|
| Go CLI skeleton (`version`, `doctor`) | builds, smoke-tested | ✅ |
| Discovery (Docker, PostgreSQL) | read-only GET/SELECT only | ✅ |
| PostgreSQL backup | snapshot-consistent, checksum, manifest, cleanup on failure | ✅ |
| Metadata (SQLite, migrations, audit) | idempotent migrations, FK, 0600 | ✅ |
| Isolated restore engine | internal network, limits, label-scoped cleanup | ✅ |
| Verification engine | VERIFIED only via `Decide()`; 10 mandatory steps | ✅ |
| JSON/text reports | scope, RTO/RPO caveats | ✅ |
| Interrupted-operation recovery | `unth cleanup` after SIGKILL | ✅ |
| M0 acceptance tests (14 scenarios) | real Docker integration tests | ✅ 14/14 (see TEST_RESULTS) |
| CI workflow | lint, race, gosec, govulncheck, integration, gitleaks | ✅ green on GitHub (run #3) |
| Docs EN + TR | consistent with CLI | ✅ (TR: README + quickstart) |
| `v0.1.0-alpha` release | see RELEASE_CRITERIA | ⬜ ready to tag (awaiting maintainer decision) |

## M1 — Self-hosted Community Edition (in progress)

| Work package | Status |
|---|---|
| Notifications (webhook + HMAC, Slack) | ✅ unit + integration tests |
| `unth run` (lock → backup → verify → prune → notify) | ✅ integration tests (success, not-verified, notify failure, overlap) |
| Retention (`unth prune`, EXPIRED) | ✅ unit (planner) + integration tests |
| Scheduling (systemd service/timer, cron) | ✅ templates; `systemd-analyze verify` clean (except binary path not installed in the dev sandbox) |
| Docker volume backup with consistency policies | ✅ offline/stop policies, archive safety, isolated restore + file comparison; integration tests |
| Install script with checksum verification | ⬜ |
| CI matrix PostgreSQL 12–17 | ✅ (done during M0 hardening) |

## M2 — Web management panel
React + TypeScript + Vite; Overview, Infrastructure, Workloads, Backups, Recovery Tests,
Reports, Policies, Activity, Settings. Authentication required; never exposed unauthenticated.
Visual flow `CREATED → INTEGRITY CHECKED → RESTORE TESTED → VERIFIED`.

## M3 — Storage & integrations
S3-compatible storage (MinIO etc.), encryption, signed manifests, Coolify & Dokploy integrations.

## M4 — Multi-server operations
Enrollment, secure transport, multi-server dashboard, RBAC, monitoring, notifications.

## M5 — Managed cloud readiness
Multi-tenancy, tenant isolation, billing architecture, hosted deployment. Separate commercial
track; Community Edition stays fully functional on its own.

## User research (before M2 investment)
Interview at least 10 self-hosters/DevOps engineers: how they test restores today, how often,
what failed, what evidence auditors ask for. M0 is the conversation starter, not the answer.

## Release targets (goals, not commitments)
`v0.1.0-alpha` PostgreSQL recovery proof · `v0.2.0-alpha` Docker workload backup ·
`v0.3.0-beta` Community Edition · `v0.4.0-beta` Web UI · `v1.0.0` production-ready
(requires load tests, recovery scenarios, external security audit, upgrade tests).
