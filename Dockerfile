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
