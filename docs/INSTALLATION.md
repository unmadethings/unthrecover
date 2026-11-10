# Installation

## Requirements
- Linux x86_64 or arm64 (M0 is tested on x86_64).
- Docker Engine reachable via a unix socket (`/var/run/docker.sock` or `DOCKER_HOST=unix://…`).
  The user running `unth` needs socket access — this is root-equivalent; see the threat model.
- PostgreSQL client tools with a major version ≥ your source server, e.g. on Ubuntu 24.04:

```bash
sudo apt-get install -y postgresql-client-16
```

- Go 1.26+ (only to build from source).

## From source
```bash
git clone https://github.com/unmadethings/unthrecover.git
```

```bash
cd unthrecover && make build
```

```bash
sudo install -m 0755 bin/unth /usr/local/bin/unth
```

## Release binaries (install script)
`scripts/install.sh` downloads the binary for your architecture (linux/amd64, linux/arm64) and
`SHA256SUMS` from a GitHub release, verifies the checksum, checks that the binary runs, and
only then installs it atomically. A mismatch or missing checksum entry aborts without
installing anything (tested in CI: `make test-install`).

```bash
curl -fsSLO https://raw.githubusercontent.com/unmadethings/unthrecover/main/scripts/install.sh
```

```bash
less install.sh
```

```bash
sudo sh install.sh --version v0.1.0-alpha
```

Options: `--dir` (default `/usr/local/bin`), `--dry-run`, `--base-url` (mirrors; `https` or
`file://` only). Without `--version` the latest *published* release is used — there is none
until the maintainers publish `v0.1.0-alpha`.

The checksum file comes from the same release as the binary, so it protects against corrupted
or swapped downloads, not against a compromised release; signed checksums are planned.

Manual verification:

```bash
sha256sum -c SHA256SUMS --ignore-missing
```

## Verify the installation
```bash
unth version
```

```bash
unth doctor
```

All checks should be `PASS`. See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) otherwise.

## Data directory
Default `~/.local/share/unthrecover` (override with `--data-dir` or `UNTH_DATA_DIR`). It holds
copies of your data — place it on a disk with enough space, readable only by the operator.

## Debian / Ubuntu package (.deb)
Release assets include `unthrecover_<version>_<arch>.deb` (listed in `SHA256SUMS`). The package
installs `/usr/bin/unth`, the systemd units `unth@.service` / `unth@.timer`, examples in
`/usr/share/doc/unthrecover/examples`, creates the system user `unth` (added to the `docker`
group if it exists), `/var/lib/unthrecover` (0700) and `/etc/unth/jobs` (0750 root:unth).
It depends on `postgresql-client`; install a client whose major version is ≥ your servers.

```bash
sha256sum -c SHA256SUMS --ignore-missing && sudo apt install ./unthrecover_0.2.0.alpha_amd64.deb   # v0.2.0-alpha; from v0.2.1: unthrecover_<version>_<arch>.deb
```

Removing the package never deletes backups (`/var/lib/unthrecover`) or job configs (`/etc/unth`).

## Container image
`ghcr.io/unmadethings/unthrecover:<version>` (linux/amd64, linux/arm64) is built from the same
release binaries. It contains PostgreSQL 17 client tools (backs up servers 12–17), runs as an
unprivileged user and needs the Docker socket — which is root-equivalent; only run it where
Docker access is already granted. During verification the container attaches itself to each
isolated sandbox network (`UNTH_SANDBOX_JOIN=auto`) and detaches again during cleanup, so it
works without host networking.

```bash
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock --group-add "$(stat -c %g /var/run/docker.sock)" -v unth-data:/var/lib/unthrecover -v "$PWD/job.yaml:/etc/unth/job.yaml:ro" -e SHOP_DB_PASSWORD ghcr.io/unmadethings/unthrecover:v0.2.0-alpha run --config /etc/unth/job.yaml
```

The source database host must be reachable from the container (use the database's service
name on a shared Docker network, or the host's address). Volume backups work the same way.
