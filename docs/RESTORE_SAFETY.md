# Restore safety

## Rule
In M0, **restores only ever target a freshly created, isolated, disposable PostgreSQL
container**. No command can restore into an existing database or volume. There is no
`--target` option.

## Sandbox properties (implemented)
- New network `unth-net-<run>`: Docker `internal` bridge (no external connectivity), ICC disabled.
- New named volume `unth-vol-<run>`; new container `unth-pg-<run>` from `postgres:<major>`.
- No published ports, no host bind mounts, not privileged, `no-new-privileges`,
  `CapDrop=ALL` + the five capabilities the official entrypoint needs.
- Memory, CPU and PIDs limits; log rotation.
- Random single-use superuser password (registered for redaction).
- Labels: `io.unthrecover.managed=true`, `io.unthrecover.run`, `io.unthrecover.backup`, `io.unthrecover.role`.

## Cleanup guarantees
- Normal completion, failure, timeout and SIGINT/SIGTERM: resources are removed before exit
  (tested: `TestInterruptDuringRestoreCleansUp`, `TestVerifyTimeout`, all failure tests).
- SIGKILL / power loss: run `unth cleanup` (tested: `TestKillDuringRestoreThenCleanup`).
- Cleanup deletes **only** resources carrying both the managed label and the specific run
  label, double-checked client-side. Global prune commands are never used.

## Future production restore (not implemented) — mandatory design requirements
1. Explicit target identification (no defaults).
2. Dry-run showing exactly what will change.
3. Interactive operator confirmation (and non-interactive token for automation).
4. Automatic pre-restore protection backup of the target.
5. Audit log entry before and after.
6. Conflict detection (connected clients, existing objects, version mismatch).
