# Offsite copies: object storage and cloud drives

A backup that lives only on the server it protects is lost together with that server.
With an `offsite:` section, `unth run` copies every **VERIFIED** backup to storage elsewhere,
**downloads it again and proves the copy**, and only then applies retention.

| Target | `type` | Examples |
|---|---|---|
| S3-compatible object storage | `s3` | AWS S3, **Google Cloud Storage**, Cloudflare R2, Backblaze B2, Wasabi, Hetzner Object Storage, DigitalOcean Spaces, MinIO, SeaweedFS, Garage |
| File storage / cloud drives through [rclone](https://rclone.org) | `rclone` | **Google Drive**, OneDrive, Dropbox, Box, pCloud, Mega, SFTP, WebDAV, Nextcloud, … (70+ providers) |

Object storage is the better choice for servers (service accounts, lifecycle rules, object
lock). Cloud drives work well for small installations and personal servers.

## What happens in `unth run`

1. backup → isolated restore test → **VERIFIED** (unchanged)
2. **copy**: the artifact and its manifest are encrypted with [age](https://age-encryption.org)
   while streaming (no plaintext temporary copy) and uploaded; then `copy.json` (hashes,
   recipients) and its Ed25519 signature `copy.json.sig`
3. **proof**: the copy is downloaded again: signature checked, stored hashes checked,
   decrypted with the key, plaintext hashes compared with the backup's recorded SHA-256 →
   `REMOTE_VERIFIED`
4. **offsite retention** (`offsite.retention`, default: the job's `retention`), then local retention
5. notifications include `offsite_target` and `offsite_status`

If any part of 2–3 fails, the run exits with **code 5**, nothing is pruned (locally or
offsite) and partial uploads are removed. A copy is never reported as stored before it was
read back and checked.

Layout under `<prefix>/<workload>/<backup-id>/`: `artifact.dump.age` (or `volume.tar.age`),
`manifest.json.age`, `copy.json`, `copy.json.sig`. `copy.json` is written last; a folder
without it is an incomplete copy and is ignored. Only `copy.json` is readable without the
key; it contains ids, times, sizes, hashes and public keys — no host names, table names or data.

## 1. Keys

```bash
sudo unth keys generate --dir /etc/unth/keys
```

| File | Purpose | Config |
|---|---|---|
| `age.key` (0600) | encryption identity; also needed to restore | `encryption.identity_file` |
| `sign.key` (0600) | signs each copy | `signing.key_file` |
| `sign.pub` | checks signatures on another machine | `signing.public_key_file` |

> **Store a copy of `age.key` outside this server** (password manager, offline media).
> Without it, encrypted copies cannot be restored — by anyone, including you.

Recommended: an **offline recovery key**. Run `unth keys generate` on another machine (or use
`age-keygen`), keep that `age.key` offline, and add its public key so every copy can also be
decrypted with it:

```yaml
encryption:
  identity_file: /etc/unth/keys/age.key
  recipients:
    - age1...   # offline recovery key
```

Encryption is mandatory for offsite copies. Plaintext copies need `offsite.allow_unencrypted: true`.

## 2a. Google Cloud Storage

1. Create a bucket (Cloud Console → Cloud Storage → Buckets → Create). Consider Object
   Versioning or a retention policy so a compromised server cannot delete old copies.
2. Create a service account with **Storage Object Admin** on that bucket only.
3. Cloud Storage → Settings → **Interoperability** → create an **HMAC key** for the service
   account. You get an access ID and a secret.
4. Put them in the environment of the job (e.g. `/etc/unth/jobs/<job>.env`, 0600, or `.env`
   with Docker Compose):
   ```
   GCS_HMAC_ID=GOOG1E...
   GCS_HMAC_SECRET=...
   ```
5. Job configuration ([example](../examples/offsite-gcs.yaml)):
   ```yaml
   offsite:
     type: s3
     endpoint: https://storage.googleapis.com
     region: auto
     bucket: my-company-backups
     prefix: servers/web1
     access_key_env: GCS_HMAC_ID
     secret_key_env: GCS_HMAC_SECRET
     retention:
       keep_daily: 30
   ```

## 2b. Other S3-compatible storage

| Provider | `endpoint` | Notes |
|---|---|---|
| AWS S3 | (omit) or `https://s3.<region>.amazonaws.com` | set `region` |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `region: auto` |
| Backblaze B2 | `https://s3.<region>.backblazeb2.com` | application key with bucket access |
| Wasabi | `https://s3.<region>.wasabisys.com` | |
| Hetzner | `https://<location>.your-objectstorage.com` | |
| MinIO / SeaweedFS / Garage | `https://minio.example.com` | `path_style: true` |

`unth` never creates buckets. `http://` is accepted for loopback addresses only, unless
`allow_http: true` (trusted internal network; data is still encrypted and requests are signed).

## 2c. Google Drive (and other drives) via rclone

1. Install rclone on the server (`sudo sh scripts/install-rclone.sh` installs the pinned,
   checksum-verified release; the container image already contains it).
2. Connect the drive once, as the user that runs unth:
   ```bash
   rclone config            # n) new remote → name: gdrive → storage: drive → follow the login
   rclone lsd gdrive:       # check
   ```
   On a server without a browser, rclone prints a command to run on your own computer
   (`rclone authorize "drive"`) and you paste the result back.
   For systemd jobs, copy the config to `/etc/unth/rclone.conf` (owner `unth`, 0600). For
   Docker Compose, mount it read-write (rclone refreshes the token in it).
3. Job configuration ([example](../examples/offsite-gdrive.yaml)):
   ```yaml
   offsite:
     type: rclone
     remote: "gdrive:Unth Backups"
     rclone_config: /etc/unth/rclone.conf
   ```

The same works for `onedrive:`, `dropbox:`, `sftp:` etc. — whatever `rclone config` created.
unth calls rclone with an argument list (no shell) and never handles the OAuth tokens itself.

## 3. Commands

```bash
unth offsite list  --config job.yaml            # complete copies on the target
unth offsite check --config job.yaml <id>       # download again and prove a copy
unth offsite push  --config job.yaml <id>       # copy an existing VERIFIED backup
unth offsite prune --config job.yaml --dry-run  # offsite retention preview
unth offsite fetch --config job.yaml <id>       # disaster recovery (below)
```

## 4. Disaster recovery: the server is gone

On a new machine with unth, Docker and the PostgreSQL client tools:

1. Restore the job file, `age.key` and (recommended) `sign.pub` from your safe place;
   set the storage credentials (or copy `rclone.conf`). In the job, use
   `signing.public_key_file: sign.pub` instead of `key_file`.
2. `unth offsite list --config job.yaml` — pick a backup.
3. `unth offsite fetch --config job.yaml <id>` — downloads, checks signature and hashes,
   decrypts and imports it as a local backup (`NOT_VERIFIED`).
4. `unth verify <id>` — restores it into an isolated container and compares it with the
   source fingerprint recorded at backup time → `VERIFIED`.
5. Restore into your new production database with `pg_restore` from
   `<data-dir>/backups/<id>/artifact.dump` (see [RESTORE_SAFETY.md](RESTORE_SAFETY.md)).

This path is covered by `TestOffsiteS3EndToEnd` (S3) and the release smoke test (rclone):
fetch on an empty data directory followed by a real isolated restore that must be VERIFIED.

## What is proven, what is not

| Check | |
|---|---|
| The copy was written completely and can be read back | ✅ after every upload |
| The copy decrypts with the configured key and equals the verified backup (SHA-256) | ✅ |
| The copy was made by this installation and not modified (Ed25519 signature) | ✅ when `signing` is set |
| A modified, truncated, swapped or unsigned copy is rejected | ✅ (tests) |
| The provider keeps the copy later (durability, deletion by others) | ❌ use bucket versioning / object lock / provider retention; `unth offsite check` re-proves on demand |
| The offline recovery key still works | ❌ test it yourself from time to time (`fetch` on a spare machine) |

Tested against: SeaweedFS 3.97 (S3 API, CI), rclone 1.75.2 with a local-storage remote (CI).
Not yet tested by the project against real Google Cloud Storage, AWS S3 or Google Drive
accounts — reports welcome.
