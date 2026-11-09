# Backup security

## What protects a backup today (M0)

| Property | Status |
|---|---|
| Integrity against accidental corruption | ✅ SHA-256, recomputed on every verification |
| Detection of non-restorable artifacts | ✅ every verification restores the backup |
| Authenticity (proof the backup was produced by you and not altered) | ❌ **not provided** |
| Confidentiality at rest | ❌ **not encrypted** |
| Access control | Filesystem only: data dir `0700`, files `0600` |

**SHA-256 is an integrity check, not an authenticity proof.** Someone who can write to the
data directory can replace an artifact and update the manifest and the metadata database
consistently. Unth Recover will still restore and compare that artifact (so a broken or
inconsistent forgery fails), but it cannot prove *who* produced it.

## Recommendations
- Keep `--data-dir` on a filesystem only the backup operator can access.
- Use a dedicated least-privilege PostgreSQL role for backups (read access to all
  backed-up objects, no write privileges).
- Copy verified backups off-host with a tool that provides encryption and immutability
  (e.g. restic or Kopia to object storage with object lock). Native support is planned.
- Store the password in `password_file` (`chmod 600`) or an environment variable provided by
  your secret manager — never in the config file (rejected).

## Planned (M3)
- Encrypted artifacts (age/X25519 or KMS-wrapped keys).
- Signed manifests (Ed25519; key outside the data dir), verified before restore.
- S3-compatible storage with object-lock awareness.
