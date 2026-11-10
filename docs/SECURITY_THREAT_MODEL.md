# Security threat model (M0)

Scope: the `unth` CLI running on a Linux host with access to the Docker socket, PostgreSQL
client tools, a source database, and a local data directory.

## Assets
1. Backup artifacts (full copies of production data).
2. Source database credentials.
3. The host (via Docker socket access — root-equivalent).
4. Integrity of the `VERIFIED` signal (operators make recovery decisions on it).
5. Production containers, volumes and databases on the same host.

## Trust boundaries
- Operator ↔ `unth` (config file, CLI args) — trusted operator, untrusted *content* is validated.
- `unth` ↔ Docker Engine (root-equivalent).
- `unth` ↔ source PostgreSQL (network, credentials).
- `unth` ↔ data directory (may be readable/writable by other local users if misconfigured).
- Restored database inside the sandbox — **untrusted** (contents come from the backup).

## Threats and mitigations

| Threat | Mitigation in M0 | Residual risk / future |
|---|---|---|
| **Command injection** | No shell is ever invoked; `exec.Command` with fixed flags. Connection parameters via `PG*` env vars, not argv; DB name never passed as conninfo. Image names validated by regex; IDs validated (`^[a-z0-9][a-z0-9-]{2,63}$`). | — |
| **SQL injection** | Identifiers quoted with `pgx.Identifier.Sanitize`. User assertion queries are operator-authored by design and run in `READ ONLY` transactions on both sides. | A malicious config author can run arbitrary read queries with the configured user — use a least-privilege backup role. |
| **Path traversal** | Paths from metadata resolved with `SafeJoin` (rejects absolute, `..`); manifest artifact path must be a bare file name; IDs validated. | — |
| **Symlink attacks** | `CheckNoSymlinks` on every component below the data dir before reading artifacts; data dir/metadata DB symlinks refused; temp files created with `O_EXCL`. | TOCTOU between check and open by a local attacker with write access to the data dir — keep it `0700`. |
| **Archive extraction vulnerabilities** | No archives are extracted on the host. `pg_restore` only reads the custom-format dump and sends SQL to the sandbox. | `pg_restore` parser bugs — keep client tools patched. |
| **Arbitrary file overwrite** | All writes go to `<data-dir>/{tmp,backups,reports}` with validated IDs; `O_EXCL` for artifacts/manifests; atomic rename. | — |
| **Credential leakage** | Passwords: never in config (rejected), never in argv, private `PGPASSFILE` (0600 in 0700 temp dir, removed after use), redactor applied to all logs, errors, stderr captures, reports and audit details. Integration test scans all output and every file in the data dir for the password. | Sandbox password is visible via `docker inspect` to Docker-socket users (who are root-equivalent anyway); it is random, single-use and the container is internal-only. |
| **Backup data exfiltration** | Data dir and files `0700/0600`. Sandbox network is `internal` (no egress) with ICC disabled; no ports published. | Artifacts are **not encrypted** (M3). Anyone with read access to the data dir has the data. |
| **Docker socket compromise** | Documented as root-equivalent; only `unix://` sockets accepted (no unauthenticated TCP). | Run `unth` on hosts where Docker access is already granted to the operator; rootless Docker is future work. |
| **Container escape** | `CapDrop=ALL` + `CHOWN, DAC_OVERRIDE, FOWNER, SETGID, SETUID` (entrypoint needs), `no-new-privileges`, no privileged mode, no host mounts, PIDs/memory/CPU limits, official PostgreSQL image. | Kernel/runtime 0-days. User-namespace remapping / gVisor support is future work. |
| **Privilege escalation** | `unth` needs no root itself; it does not chmod/chown outside its data dir. | Docker group membership is itself privileged. |
| **Unauthorized restore** | There is no command that restores into anything other than a fresh sandbox. Production restore is not implemented. | When added (M1+): explicit target, dry-run, confirmation, pre-restore snapshot, audit. |
| **Backup tampering** | SHA-256 checked against the manifest **and** the separate metadata DB; fingerprint digest cross-checked; the full restore + data comparison detects archives that don't restore or don't match. | **Checksums are not authenticity.** An attacker who can rewrite artifact, manifest *and* metadata DB can forge a consistent backup. Signed manifests planned (M3). |
| **Storage exhaustion** | Pre-flight `min_free_bytes`; ENOSPC during dump → `INSUFFICIENT_STORAGE`, partial file removed (tested on a tiny tmpfs). | No retention yet (M1). |
| **Resource exhaustion** | Sandbox memory/CPU/PIDs limits, shm 256 MiB, log rotation; whole-run timeout; backup timeout; bounded reads of manifests (32 MiB), configs (1 MiB), tool stderr (64 KiB), Docker responses. | Restore volume size is not capped (depends on Docker storage driver). |
| **Malicious backup artifacts** | Sandbox isolation (above); restored DB treated as untrusted; health/fingerprint queries are read-only and do not execute functions from the restored DB other than through standard catalog functions. | Restored triggers/functions could be crafted to consume CPU during checks — bounded by timeout and limits. |
| **Insecure temporary files** | Partial artifacts in `<data-dir>/tmp` (0700) with `O_EXCL`; pgpass in `os.MkdirTemp` dir chmod 0700; removed on all paths. | — |
| **Notification secret leakage / SSRF** | Webhook URLs and HMAC keys only from env vars, redacted everywhere; errors show the host only; https required (http only to loopback); no redirects; responses bounded to 64 KiB; payload contains no data rows or credentials. | URL is operator-controlled; an operator can point it at an internal https service — intended. |
| **Overlapping scheduled runs** | Per-job `flock` in `<data-dir>/locks`; second run exits 1 before doing anything (tested). | — |
| **Retention deleting the only good backup** | Newest VERIFIED backup never expired; prune skipped when the run's verification failed; dry-run; audit events (tested). | Deleting artifacts is irreversible by design. |
| **Orphaned resources / wrong cleanup target** | Cleanup only removes resources with BOTH managed + exact run label, re-checked client-side (unit test with a filter-ignoring fake Engine). `unth cleanup` reconciles after SIGKILL. | PID reuse could make a dead run look alive (cleanup then skips it — safe direction). |
| **False VERIFIED** | `Decide()` pure function; checksum-only and partial runs cannot pass (unit tests); integration tests for corruption with *consistent* checksums, data mismatch, manifest tampering. | Data not covered by the scope (roles, large tables' row content above the hash threshold) — stated in every report. |

## Security testing performed
See [TEST_RESULTS.md](TEST_RESULTS.md): gosec (0 findings after triage), staticcheck, go vet,
race detector, gitleaks (no leaks), secret-leak integration test.
