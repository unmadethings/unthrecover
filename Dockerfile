# Unth Recover container image.
#
# Built from release binaries (make dist) so the image contains exactly the
# tested artifact:   make dist VERSION=vX && docker build --build-arg VERSION=vX .
#
# The base provides PostgreSQL 17 client tools (pg_dump/pg_restore 17 can back up
# servers 12–17). unth needs the Docker socket (root-equivalent!) and joins its
# own isolated sandbox networks during verification (UNTH_SANDBOX_JOIN=auto).
#
#   docker run --rm \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     --group-add "$(stat -c %g /var/run/docker.sock)" \
#     -v unth-data:/var/lib/unthrecover \
#     -v "$PWD/job.yaml:/etc/unth/job.yaml:ro" \
#     ghcr.io/unmadethings/unthrecover:<version> run --config /etc/unth/job.yaml
ARG BASE=postgres:17-alpine
FROM ${BASE}
ARG TARGETARCH=amd64
ARG VERSION=dev
LABEL org.opencontainers.image.title="Unth Recover" \
      org.opencontainers.image.description="Backup + proven recovery for PostgreSQL and Docker volumes" \
      org.opencontainers.image.source="https://github.com/unmadethings/unthrecover" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
COPY dist/unth_${VERSION}_linux_${TARGETARCH} /usr/local/bin/unth
RUN addgroup -S -g 10001 unth && adduser -S -D -H -u 10001 -G unth unth \
 && mkdir -p /var/lib/unthrecover /etc/unth \
 && chown unth:unth /var/lib/unthrecover && chmod 0700 /var/lib/unthrecover \
 && chmod 0755 /usr/local/bin/unth && /usr/local/bin/unth version
USER 10001:10001
ENV UNTH_DATA_DIR=/var/lib/unthrecover \
    UNTH_SANDBOX_JOIN=auto
VOLUME ["/var/lib/unthrecover"]
ENTRYPOINT ["/usr/local/bin/unth"]
CMD ["doctor"]
