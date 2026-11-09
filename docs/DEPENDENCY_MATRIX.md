# Dependency matrix

## Go modules compiled into `unth`

Audited on 2026-11-09 from `go list -deps ./cmd/unth` and each module's LICENSE file.

| Module | Version | License | Purpose |
|---|---|---|---|
| github.com/spf13/cobra | v1.8.1 | Apache-2.0 | CLI |
| github.com/spf13/pflag | v1.0.5 | BSD-3-Clause | flags (cobra) |
| github.com/jackc/pgx/v5 | v5.12.0 | MIT | PostgreSQL driver for fingerprints/health |
| github.com/jackc/pgpassfile, pgservicefile | — | MIT | pgx deps |
| gopkg.in/yaml.v3 | v3.0.1 | MIT / Apache-2.0 | config |
| modernc.org/sqlite (+ libc, mathutil, memory) | v1.34.1 | BSD-3-Clause | metadata DB (pure Go) |
| github.com/dustin/go-humanize | v1.0.1 | MIT | sqlite dep |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause | sqlite dep |
| github.com/remyoudompheng/bigfft | — | BSD-3-Clause | sqlite dep |
| golang.org/x/crypto, x/sys, x/text | v0.58.0 / v0.50.0 / v0.44.0 | BSD-3-Clause | pgx / sqlite deps |

All licenses are permissive and compatible with Apache-2.0. `go mod verify` runs in CI.

## External programs (not linked)

| Component | Tested version | Requirement |
|---|---|---|
| PostgreSQL client tools (`pg_dump`, `pg_restore`) | 16.15 | major ≥ source server major |
| PostgreSQL server (source) | 16.15 (alpine), 17 → controlled failure | 12 … pg_dump major |
| Restore image | `postgres:16`, `postgres:16-alpine` | must run the official entrypoint |
| Docker Engine | 29.9.2 (API 1.56) | API ≥ 1.40 and unix socket |
| Linux kernel | 6.18 (CI: ubuntu-24.04) | |

## Vulnerability scanning

`govulncheck` runs in CI (`make vuln`). It could not run in the M0 development sandbox
(`vuln.go.dev` blocked, HTTP 403). Its first CI run found three reachable vulnerabilities —
GO-2026-5004 (pgx 5.7.5), GO-2026-6629 and GO-2026-5970 (x/text 0.27.0) — fixed by upgrading
to pgx 5.12.0 and x/text 0.44.0.
