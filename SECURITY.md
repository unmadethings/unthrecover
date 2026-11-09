# Security policy

## Supported versions
Unth Recover is pre-release (`0.x`). Security fixes are made on `main` and in the latest
pre-release only.

## Reporting a vulnerability
**Do not open a public issue.** Report privately via GitHub:
https://github.com/unmadethings/unthrecover/security/advisories/new

Please include affected version/commit, a description, reproduction steps and impact. Never
include real credentials or production data. We aim to acknowledge reports within 5 working
days and to agree on a disclosure timeline with you.

## Scope highlights
Of particular interest: anything that can mark a non-restorable backup as `VERIFIED`, touch
resources outside the sandbox, leak credentials, escape the sandbox, or cause path traversal /
file overwrite. See [docs/SECURITY_THREAT_MODEL.md](docs/SECURITY_THREAT_MODEL.md).

## Known, documented limitations (not vulnerabilities)
- Backups are not encrypted; SHA-256 checksums are not signatures.
- Docker socket access is root-equivalent.
See [docs/BACKUP_SECURITY.md](docs/BACKUP_SECURITY.md).
