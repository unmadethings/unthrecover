# Product vision

**Unth Recover** — open-source disaster recovery & backup verification platform by Unmade Things.

> Your backup is not your recovery. Don't just back up — prove you can recover.

## Problem
A backup job that "succeeded" proves only that a file was written. Recovery fails in
practice because of truncated or corrupt artifacts, missing extensions, incompatible
versions, objects that fail to load, or backups that never contained what people assumed.
Recovery testing is widely recommended and rarely automated for self-hosted stacks.

## Product principle
A backup is `VERIFIED` only after it was actually restored into an isolated environment and
the restored state was checked against what was backed up. Every report states exactly what
was and was not verified.

## Users
Self-hosted developers, Docker users, Coolify/Dokploy users, sysadmins, DevOps engineers,
small/medium SaaS teams running their own servers.

## Recovery chain
Discover → define policy → back up → check integrity → create isolated environment →
restore → run database/service checks → prove success → auditable report → report
restore duration and data-loss exposure.

## Independence and business model
- Community Edition: fully self-hosted, Apache-2.0, no account, no phone-home, no dependency on
  any commercial Unmade Things product or closed API.
- Managed Cloud (future, M5): optional hosted control plane, monitoring, notifications.
- Enterprise services (future): support, integrations, managed recovery drills.
- GitHub Sponsors: once enabled.
Core backup and verification will never require the paid service.

## Non-goals (for now)
Kubernetes operators, distributed consensus, microservices, payment/licensing code.
