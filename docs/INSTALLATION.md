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

## Release binaries
No release has been published yet. When `v0.1.0-alpha` is published, binaries for
linux/amd64 and linux/arm64 plus `SHA256SUMS` will be attached. Verify before installing:

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
