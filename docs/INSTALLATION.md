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
