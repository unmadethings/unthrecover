# Backup security

## What protects a backup

| Property | Status |
|---|---|
| Integrity against accidental corruption | ✅ SHA-256, recomputed on every verification |
| Detection of non-restorable artifacts | ✅ every verification restores the backup |
| Authenticity of offsite copies (Ed25519-signed `copy.json` with the hashes) | ✅ with `signing:` ([OFFSITE.md](OFFSITE.md)) |
| Authenticity of the local data directory | ❌ not provided (an attacker with write access to it can forge a consistent backup) |
| Confidentiality of offsite copies | ✅ age (X25519) encryption before upload; mandatory unless `allow_unencrypted` |
| Confidentiality of the local data directory | ❌ not encrypted — protected by file permissions (`0700`/`0600`) |
| Access control | Filesystem only: data dir `0700`, files `0600` |

**SHA-256 is an integrity check, not an authenticity proof.** Someone who can write to the
data directory can replace an artifact and update the manifest and the metadata database
consistently. Unth Recover will still restore and compare that artifact (so a broken or
inconsistent forgery fails), but it cannot prove *who* produced it.

## Recommendations
- Keep `--data-dir` on a filesystem only the backup operator can access.
- Use a dedicated least-privilege PostgreSQL role for backups (read access to all
  backed-up objects, no write privileges).
- Configure `offsite:` so every verified backup also exists, encrypted and signed, outside
  the server; enable bucket versioning / object lock on the provider side.
- Keep a copy of the age key and an offline recovery key away from the server.
- Store the password in `password_file` (`chmod 600`) or an environment variable provided by
  your secret manager — never in the config file (rejected).

## Planned
- Optional encryption of the local data directory.
- KMS-wrapped keys; object-lock retention set by unth.
