# Competitive and need analysis (initial, desk research)

Date: 2026-11-09. This is desk research to position M0, **not** user validation. Claims are
linked to sources; items marked *(not re-checked)* come from prior knowledge and were not
re-verified in this session.

## What existing tools already solve

| Tool | What it does well | Restore verification |
|---|---|---|
| **Coolify** | Scheduled, engine-aware DB dumps (PostgreSQL, MySQL, MariaDB, MongoDB, …), S3 upload, retention, import/restore UI. [docs](https://coolify.io/docs/databases/backups) | Its docs explicitly say a successful backup only proves a file was created and recommend restoring into a disposable database to verify — **manual**. Dumps → no PITR. [docs](https://coolify.io/docs/databases/backups), [instance backup](https://coolify.io/docs/core/backup-and-recovery/instance-backup) |
| **Dokploy** | Scheduled DB backups to S3 destinations and one-click restore into a named database. [backups](https://docs.dokploy.com/docs/core/databases/backups), [restore](https://docs.dokploy.com/docs/core/databases/restore) | No automated restore test found in the docs; third-party guides recommend testing restores in a separate database ([example](https://www.mintlify.com/Dokploy/dokploy/databases/backups)). |
| **restic** | Encrypted, deduplicated file backups to many backends. `check` verifies repository structure; `check --read-data` reads all pack data. [manpage](https://manpages.opensuse.org/Tumbleweed/restic/restic-check.1.en.html), [docs](https://restic.readthedocs.io/en/v0.9.6/045_working_with_repos.html) | Verifies the *repository*, not that an application/database restored from it works. |
| **Kopia** | Encrypted, deduplicated snapshots, UI, policies. `snapshot verify` *(not re-checked)* | Repository/content verification; no application-level restore test. |
| **BorgBackup** | Deduplicated, encrypted archives; `borg check --verify-data` *(not re-checked)* | Same as above. |
| **pgBackRest** | Physical PostgreSQL backups, WAL archiving, PITR, parallelism; `verify` command validates backups and archives in the repository. [verify](https://pigsty.io/docs/pgbackrest/command/verify/index.md) | Checks files/checksums; a restore test is still a separate operator task. |
| **pg_verifybackup** | Verifies `pg_basebackup` output against its manifest. [man](https://www.mankier.com/1/pg_verifybackup) | File-level; does not start a server or compare data. |

## Where Unth Recover can be better
1. **Automated real restore on every verification** into a fresh isolated container, with
   cleanup — the step every tool above leaves to the operator.
2. **Expected-state comparison**: the restored DB is compared with a fingerprint taken in the
   *same snapshot* as the dump (row counts, content hashes, schema, constraints, custom SQL).
   Repository checks cannot detect "the dump restores but is missing data/objects".
3. **Honest evidence**: explicit verification scope, separate observed restore time vs RTO,
   exposure window vs RPO — suitable as audit evidence.
4. **Complementary**: Unth Recover can verify dumps that are then shipped off-site by restic /
   Kopia / Borg, and (future) verify pgBackRest/WAL-based restores.

## Open questions for user interviews
- How often do teams test restores today, and what triggers it?
- What evidence do customers/auditors ask for?
- Is per-backup verification affordable for large DBs, or is sampling (e.g. weekly) enough?
- Which integration matters most: Coolify, Dokploy, plain Docker Compose, Kubernetes?

## Sources
- https://coolify.io/docs/databases/backups
- https://coolify.io/docs/core/backup-and-recovery/instance-backup
- https://docs.dokploy.com/docs/core/databases/backups
- https://docs.dokploy.com/docs/core/databases/restore
- https://www.mintlify.com/Dokploy/dokploy/databases/backups
- https://manpages.opensuse.org/Tumbleweed/restic/restic-check.1.en.html
- https://restic.readthedocs.io/en/v0.9.6/045_working_with_repos.html
- https://pigsty.io/docs/pgbackrest/command/verify/index.md
- https://www.mankier.com/1/pg_verifybackup
