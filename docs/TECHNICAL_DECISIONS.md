# Technical decisions

Each decision records context, choice and consequences. Ordered by the project's
priority list (data safety → verification capability → self-hosting → simplicity → …).

## TD-001 Go, single binary, modular monolith
Single static binary (`CGO_ENABLED=0`), easy Linux distribution, good Docker/PostgreSQL
ecosystem, approachable for contributors. No microservices, no daemon in M0.

## TD-002 Docker Engine REST API over the unix socket instead of the Docker Go SDK
The official Engine API is used directly through a ~400-line client
(`internal/docker`). The Go SDK (`github.com/docker/docker` / `moby/moby/client`) pulls a
large dependency tree and has changed module paths; we need ~15 endpoints. A smaller
supply-chain surface matters for a tool that holds root-equivalent socket access. The
client negotiates the API version (capped at the newest tested, raised to the daemon
minimum). Tested against Engine 29.9.2 (API 1.56, negotiated 1.51).
Consequence: new endpoints must be added by hand. `tcp://` hosts are refused.

## TD-003 Official PostgreSQL tools for dump/restore, pgx for checks
`pg_dump`/`pg_restore` are the reference implementation of logical backup. Custom format
enables `pg_restore --list` validation and single-transaction restore. Fingerprints and
health checks use `jackc/pgx` (MIT) so queries are parameterised and connection settings
are set field-by-field.

## TD-004 Snapshot-consistent fingerprint (`pg_export_snapshot` + `pg_dump --snapshot`)
Comparing a restore to the *live* source would produce false failures under writes. The
fingerprint is captured in the same exported snapshot that `pg_dump` uses, so the expected
state is exactly the dumped state. Integration test `TestConcurrentWritesDuringBackupStillVerify`
covers this. **Sequences are non-transactional** in PostgreSQL; their values are checked for
monotonicity (restored ≥ captured), not equality — discovered by that same test.

## TD-005 Restore runs on the host against an isolated container
Alternatives: run `pg_restore` inside the container (needs the artifact mounted into the
container and a `pg_restore` version that can read the archive). Chosen: host `pg_restore`
(same version that validated the archive) over an internal Docker network to a container
whose major version matches the source. Consequence: Linux-only in M0 (Docker Desktop does
not route to bridge IPs).

## TD-006 Default restore image `postgres:<major>` (Debian)
Debian images ship common libc locales, avoiding false failures for databases using
`en_US.utf8` collations. Alpine can be selected via `verification.restore_image`.

## TD-007 SQLite via `modernc.org/sqlite` (pure Go)
No cgo, keeps the static binary. WAL + `synchronous=FULL` + foreign keys. Forward-only,
append-only migrations in `internal/metadata`; a binary refuses a newer schema.

## TD-008 Passwords via `PGPASSFILE`, connection via `PG*` environment variables
Passwords never appear in argv (visible in `ps`) or the child environment. Database names
are never passed as a `--dbname` connection string from user input (conninfo injection);
ambient `PGPASSWORD`/`PGSERVICE` are not inherited.

## TD-009 Label-scoped cleanup with defence in depth
Every resource gets `io.unthrecover.managed=true` and `io.unthrecover.run=<run-id>`. Cleanup
filters by both labels **and** re-checks labels client-side before deleting
(`TestCleanupOnlyRemovesOwnLabeledResources` uses a fake Engine that ignores filters).

## TD-010 Go 1.25 minimum
`go mod tidy` with the selected `modernc.org/sqlite`/`x/*` versions requires `go 1.25`.
Local toolchain: 1.24.7 auto-switched to 1.25.0.

## TD-011 License: Apache-2.0
All direct and transitive dependencies are MIT, BSD-2/3 or Apache-2.0 (see
[DEPENDENCY_MATRIX.md](DEPENDENCY_MATRIX.md)); no copyleft obligations, compatible.
PostgreSQL and Docker are invoked as external programs/APIs, not linked.
