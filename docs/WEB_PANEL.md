# Web management panel (`unth serve`)

A self-hosted, authenticated panel on top of the same data directory the CLI and `unth run` use.

```bash
openssl rand -hex 32 > ui.token && chmod 600 ui.token
```

```bash
unth serve --token-file ui.token            # http://127.0.0.1:8484
```

Remote access: use TLS (`--listen 0.0.0.0:8443 --tls-cert cert.pem --tls-key key.pem`) or keep it on
loopback behind an SSH tunnel / reverse proxy that terminates TLS. Plain HTTP on a non-loopback
address is refused.

## Views
- **Overview** — protected workloads, last successful backup, last *verified* recovery, 30-day
  recovery success rate, failed verifications, storage, average observed test-restore time, and a
  per-workload table (workloads without a verified backup are highlighted).
- **Backups & recovery tests** — every backup with the pipeline
  `Created → Integrity checked → Restore tested → Verified`, verification status and size.
- **Backup detail** — facts, **Verify now** (runs the same real isolated-restore verification as
  `unth verify`, in the background, one at a time per backup), all verification runs, and the full
  report: final status, failure category, **suggested fix**, every step, what was and was **not**
  verified, observed restore time vs RTO and data-loss window vs RPO notes, cleanup.
- **Activity log** — audit trail (backups, verifications, prunes, container stops/restarts, UI logins).
- **Settings** — version, data directory, free space, Docker status, transport.

Light and dark themes follow the operating system.

## Security
Token login (constant-time compare, ≥24 chars, from a 0600 file or `UNTH_UI_TOKEN`); HttpOnly,
SameSite=Strict session cookies (Secure under TLS, or behind a declared proxy when it reports
`X-Forwarded-Proto: https`) valid for 12 h; state-changing API calls need
the `X-Unth-CSRF: 1` header; 5 failed logins per minute per address → HTTP 429; CSP `default-src
'self'`, `frame-ancestors 'none'`, `nosniff`, `no-referrer`; all logins and UI-started
verifications are audited. There is a single admin role in this release (RBAC is planned for M4).

## API (JSON, same session)
`GET /api/overview`, `GET /api/backups[?workload=]`, `GET /api/backups/{id}`,
`POST /api/backups/{id}/verify`, `GET /api/runs/{id}/report`, `GET /api/audit`, `GET /api/info`,
`POST /api/login`, `POST /api/logout`, `GET /api/session`.

## Development
The UI lives in `web/` (React + TypeScript + Vite) and is built into `internal/web/dist`, which
`go build` embeds:

```bash
make web
```

## Behind a reverse proxy (Compose, Coolify, Dokploy)
`unth serve --listen 0.0.0.0:8484 --behind-proxy` serves plain HTTP inside a container and leaves
TLS to the proxy. The Compose file publishes the port on `127.0.0.1` only. The client address used
for login rate limiting is the proxy's (forwarded headers are not trusted for it), which makes the
limit stricter, not weaker.
