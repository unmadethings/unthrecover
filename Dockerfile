# Unth Recover container image.
#
# Built from release binaries (make dist) so the image contains exactly the
# tested artifact:   make dist VERSION=vX && docker build --build-arg VERSION=vX .
#
# PostgreSQL client tools 13–17 are installed side by side from the PGDG
# repository (/usr/lib/postgresql/<N>/bin); unth automatically uses the pair
# matching each source server's major version (pg_restore 17 cannot restore
# into PostgreSQL 16 and older). Servers 12 use the 13 clients.
#
# rclone (for offsite copies to Google Drive, OneDrive, Dropbox, ...) is the
# official release binary, pinned by version and SHA-256 below.
#
# unth needs the Docker socket (root-equivalent!) and joins its own isolated
# sandbox networks during verification (UNTH_SANDBOX_JOIN=auto):
#
#   docker run --rm \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     --group-add "$(stat -c %g /var/run/docker.sock)" \
#     -v unth-data:/var/lib/unthrecover \
#     -v "$PWD/job.yaml:/etc/unth/job.yaml:ro" \
#     ghcr.io/unmadethings/unthrecover:<version> run --config /etc/unth/job.yaml
ARG BASE=debian:bookworm-slim
FROM ${BASE}
ARG TARGETARCH=amd64
ARG VERSION=dev
# Space-separated client majors to install from PGDG; empty = keep the base image's tools.
ARG PG_CLIENTS="13 14 15 16 17"
# Empty = no rclone (local test builds). Hashes from the release's SHA256SUMS.
ARG RCLONE_VERSION=1.75.2
ARG RCLONE_SHA256_amd64=349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702
ARG RCLONE_SHA256_arm64=7e1e8d69654941b7b7df84ee74c5f7fb09cce7d5496947fb7bf16b55ff427d10
LABEL org.opencontainers.image.title="Unth Recover" \
      org.opencontainers.image.description="Backups with proven recovery for PostgreSQL and Docker volumes" \
      org.opencontainers.image.source="https://github.com/unmadethings/unthrecover" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
RUN set -eu; \
    if [ -n "$PG_CLIENTS" ]; then \
      apt-get update; \
      apt-get install -y --no-install-recommends ca-certificates postgresql-common; \
      /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y; \
      pkgs=""; for v in $PG_CLIENTS; do pkgs="$pkgs postgresql-client-$v"; done; \
      apt-get install -y --no-install-recommends $pkgs; \
      rm -rf /var/lib/apt/lists/*; \
    fi; \
    if [ -n "$RCLONE_VERSION" ]; then \
      case "$TARGETARCH" in amd64) sum="$RCLONE_SHA256_amd64" ;; arm64) sum="$RCLONE_SHA256_arm64" ;; *) exit 1 ;; esac; \
      apt-get update; apt-get install -y --no-install-recommends ca-certificates curl unzip; \
      zip="rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}.zip"; \
      curl -fsSL -o "/tmp/$zip" "https://github.com/rclone/rclone/releases/download/v${RCLONE_VERSION}/$zip"; \
      echo "$sum  /tmp/$zip" | sha256sum -c -; \
      unzip -q -d /tmp/rclone "/tmp/$zip"; \
      install -m 0755 "/tmp/rclone/rclone-v${RCLONE_VERSION}-linux-${TARGETARCH}/rclone" /usr/local/bin/rclone; \
      rm -rf /tmp/rclone "/tmp/$zip"; \
      apt-get purge -y curl unzip; apt-get autoremove -y; rm -rf /var/lib/apt/lists/*; \
      rclone version | head -1; \
    fi; \
    useradd --system --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin unth; \
    mkdir -p /var/lib/unthrecover /etc/unth; \
    chown unth:unth /var/lib/unthrecover; chmod 0700 /var/lib/unthrecover
COPY dist/unth_${VERSION}_linux_${TARGETARCH} /usr/local/bin/unth
RUN chmod 0755 /usr/local/bin/unth && /usr/local/bin/unth version
USER 10001:10001
ENV UNTH_DATA_DIR=/var/lib/unthrecover \
    UNTH_SANDBOX_JOIN=auto
VOLUME ["/var/lib/unthrecover"]
ENTRYPOINT ["/usr/local/bin/unth"]
CMD ["doctor"]
