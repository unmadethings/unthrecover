# Offsite copies: object storage and cloud drives

A backup that lives only on the server it protects is lost together with that server.
With an `offsite:` section, `unth run` copies every **VERIFIED** backup to storage elsewhere,
**downloads it again and proves the copy**, and only then applies retention.

| Target | `type` | Examples |
|---|---|---|
| S3-compatible object storage | `s3` | AWS S3, **Google Cloud Storage**, Cloudflare R2, Backblaze B2, Wasabi, Hetzner Object Storage, DigitalOcean Spaces, MinIO, SeaweedFS, Garage |
| **Google Drive**, connected in the panel | `gdrive` | your own Drive, OAuth in the browser |
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

Each copy gets a readable, dated folder, by month:

```
<prefix>/<workload>/2026-11/2026-11-11 17.30/
<prefix>/<workload>/2026-11/2026-11-11 17.30-2/   (second backup in the same minute)
```

The time is the backup's creation time in `offsite.time_zone` (an IANA name such as
`Europe/Istanbul`; default: the server's time zone). Copies made before v0.5 stay in
`<prefix>/<workload>/<backup-id>/` and remain readable, checkable and fetchable. What a folder
holds is decided by its signed `copy.json` (backup id, job), never by the folder name, so
renaming or moving copies cannot pass one backup off as another.

Each folder holds `artifact.dump.age` (or `volume.tar.age`),
`manifest.json.age`, `copy.json`, `copy.json.sig`. `copy.json` is written last; a folder
without it is an incomplete copy and is ignored. On Google Drive, folders emptied by
retention are removed too. Only `copy.json` is readable without the
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

## 2c. Google Drive — connect in the panel (recommended)

Each user connects **their own** Google Drive; nothing to install, no rclone, no desktop app.

1. Open the panel (`unth serve`) at `http://127.0.0.1:8484`. For a remote server, use an SSH
   tunnel from your computer: `ssh -L 8484:127.0.0.1:8484 your-server`, then open
   `http://127.0.0.1:8484` in your browser.
2. **Settings → Connections → Connect Google Drive.** Google's own sign-in page opens; choose
   the account and click **Allow**. You come back to the panel: *connected as "gdrive"*.
3. Job configuration:
   ```yaml
   offsite:
     type: gdrive
     connection: gdrive
   ```

What unth can access: only the files it creates itself (Google's `drive.file` permission) —
they are in the folder **Unth Recover Backups**. The rest of your Drive is invisible to it.
Your Google password is never seen by unth (Google does not allow apps to sign in with it).
The access token is stored on your server only (`<data-dir>/connections/gdrive.json`, 0600),
never shown by the panel and never sent anywhere but Google. **Disconnect** in the panel revokes
it at Google; you can also remove "Unth Recover" in your Google account's security settings.

**Use an existing folder.** Next to the connection, **Choose folder…** opens Google's own
folder picker (also shared drives). The folder you pick — and only that folder — is shared
with unth; new copies go into it (`<folder>/<job>/2026-11/2026-11-11 17.30/`). If that folder is
later deleted, put in the trash or unshared, copies fail with a clear message instead of
silently going somewhere else; **Use default** goes back to *Unth Recover Backups*. On a shared
drive where you may not delete files for good, retention moves old copies to the trash.

On a new machine after a disaster, connect the same Google account again: unth finds the
existing *Unth Recover Backups* folder (or choose the same folder again), and
`unth offsite fetch` brings copies back.

The project's OAuth client is built into release binaries. Self-built binaries, or
organizations that want their own name on Google's consent screen, set
`UNTH_GOOGLE_CLIENT_ID` and `UNTH_GOOGLE_CLIENT_SECRET` (a Google Cloud "Desktop app" OAuth
client with the Drive API enabled; publish it "In production" so tokens do not expire after 7
days). The folder picker also needs a browser API key restricted to the **Google Picker API**
(`UNTH_GOOGLE_API_KEY`; no website restriction — the panel sends no referrer); without it the
panel offers only the default folder. While the picker is available, the panel's
Content-Security-Policy also allows `apis.google.com` scripts and a `docs.google.com` frame.
A panel reached only through a domain needs an own "Web application" client with
`UNTH_GOOGLE_REDIRECT_URL=https://<panel>/oauth/google/callback` — or the SSH tunnel above.

## 2d. Google Drive and other drives via rclone

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

Tested against: SeaweedFS 3.97 (S3 API, CI), rclone 1.75.2 with a local-storage remote (CI), and a
protocol-faithful fake of Google OAuth + Drive v3 (PKCE, single-use codes, token refresh,
revocation, resumable uploads, drive.file visibility), including a browser click on
"Connect Google Drive" (CI).
Not yet tested by the project against real Google Cloud Storage, AWS S3 or Google Drive
accounts — reports welcome.
