# Agent: connecting a server to Unth Recover Cloud

`unth agent` connects an installation to a control plane — Unth Recover Cloud or a self-hosted
one — so many servers can be managed from one web panel. The agent is part of the open-source
CLI; everything it does also works without it (`unth run`, `unth schedule`, systemd timers).

```bash
# "Add server" in the panel shows a one-time token (valid 1 hour)
sudo -u unth env UNTH_DATA_DIR=/var/lib/unthrecover UNTH_ENROLL_TOKEN=<token> \
  unth agent enroll --url https://app.unthrecover.com
sudo systemctl enable --now unth-agent      # packages install the unit
```

## What the agent does
- **Heartbeat** every 30 s (outbound HTTPS only; works behind NAT and firewalls): version,
  Docker status, free space, running jobs.
- **Managed jobs**: syncs the jobs defined in the panel and runs them on their schedule
  (`daily HH:MM`, `every 6h`) — **also while the control plane is unreachable**.
- **Commands**: *Run now*, *Verify now*, *Sync*.
- **Events**: the result of every run/verification — summaries only.

## Security model
| | |
|---|---|
| Authentication | Enrollment with a one-time token, then every request is signed with the server's own Ed25519 key (`<data-dir>/agent/sign.key`, 0600): method, path, timestamp (±5 min), random nonce (replay protection), SHA-256 of the body. No reusable secret is stored on either side. |
| What reaches the control plane | Hostname, versions, free space; per run: outcome, statuses, ids, sizes, durations, error category/message (redacted). **Never** backup data, table contents or passwords. |
| Secrets (database passwords, storage keys) | Entered in the panel, encrypted in the browser/control plane to **this server's age key** (`age.key`); the control plane stores only ciphertext. The agent decrypts them only for the duration of a run (environment of the run, removed afterwards); on disk they stay encrypted (`jobs.json`). |
| Limits on managed jobs | A job from the control plane may **not** read local files (`password_file`), choose key files, or set `rclone_binary`/`rclone_config`; offsite copies always use the agent's own keys and are always encrypted. Rejected jobs are reported (`job.rejected`). |
| Compromised control plane | Can define jobs that back up databases the server can reach and run verification containers (as an operator could) — but cannot read the server's files, decrypt existing offsite copies without the server's key, or impersonate the server. |

The protocol is public: [`pkg/agentproto`](../pkg/agentproto) (message types and request
signature). Tested against a reference control plane (`internal/fakecloud`): enrollment,
signature/replay/clock-skew rejection, job policy, secret handling, outage (events are kept and
sent later), and with a real database — a managed job run by command → VERIFIED, password never
in events, logs or files (`TestAgentManagedJob`).
