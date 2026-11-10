#!/bin/sh
# Installs the built .deb in a clean Debian-based container and checks the result.
#   scripts/test-deb.sh <path-to-deb> [image]
# Default image: postgres:16 (Debian bookworm, already ships postgresql-client-16).
set -eu
DEB="$1"
IMAGE="${2:-postgres:16}"
[ -f "$DEB" ] || { echo "missing $DEB" >&2; exit 1; }
docker run --rm --network none --label io.unthrecover.test=deb \
  -v "$(cd "$(dirname "$DEB")" && pwd)/$(basename "$DEB"):/tmp/unth.deb:ro" \
  --entrypoint sh "$IMAGE" -ec '
    groupadd -f docker
    if ! dpkg -i /tmp/unth.deb; then
      echo "dpkg -i reported errors" >&2; exit 1
    fi
    test "$(stat -c %a /)" = "755" || { echo "package changed / permissions"; exit 1; }
    getent passwd unth >/dev/null || { echo "user unth missing"; exit 1; }
    id -nG unth | grep -qw docker || { echo "unth not in docker group"; exit 1; }
    test "$(stat -c "%a %U" /var/lib/unthrecover)" = "700 unth" || { echo "bad data dir perms: $(stat -c "%a %U" /var/lib/unthrecover)"; exit 1; }
    test "$(stat -c "%a %U:%G" /etc/unth/jobs)" = "750 root:unth" || { echo "bad jobs dir perms"; exit 1; }
    grep -q "^ExecStart=/usr/bin/unth run" /lib/systemd/system/unth@.service || { echo "unit does not use /usr/bin/unth"; exit 1; }
    test -f /lib/systemd/system/unth@.timer
    /usr/bin/unth version
    su -s /bin/sh unth -c "unth --data-dir /var/lib/unthrecover backup list" >/dev/null
    dpkg -r unthrecover >/dev/null
    test -d /var/lib/unthrecover || { echo "removal deleted backups dir"; exit 1; }
    test ! -e /usr/bin/unth
    echo "DEB TEST: PASS"
  '
