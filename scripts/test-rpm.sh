#!/bin/sh
# Installs the built .rpm in a clean RPM-based container and checks the result.
#   scripts/test-rpm.sh <path-to-rpm> [image]
# Default image: rockylinux:9. Runs without network: no repository is used.
set -eu
RPM="$1"
IMAGE="${2:-rockylinux:9}"
[ -f "$RPM" ] || { echo "missing $RPM" >&2; exit 1; }
docker run --rm --network none --label io.unthrecover.test=rpm \
  -v "$(cd "$(dirname "$RPM")" && pwd)/$(basename "$RPM"):/tmp/unth.rpm:ro" \
  --entrypoint sh "$IMAGE" -ec '
    getent group docker >/dev/null || groupadd docker
    rpm -qp --qf "%{NAME} %{VERSION}-%{RELEASE} %{ARCH}\n" /tmp/unth.rpm
    rpm -ivh --nodeps /tmp/unth.rpm
    rpm -V unthrecover || { echo "rpm -V reported changes"; exit 1; }
    test "$(stat -c %a /)" = "555" -o "$(stat -c %a /)" = "755" || { echo "package changed / permissions"; exit 1; }
    getent passwd unth >/dev/null || { echo "user unth missing"; exit 1; }
    id -nG unth | grep -qw docker || { echo "unth not in docker group"; exit 1; }
    test "$(stat -c "%a %U" /var/lib/unthrecover)" = "700 unth" || { echo "bad data dir perms: $(stat -c "%a %U" /var/lib/unthrecover)"; exit 1; }
    test "$(stat -c "%a %U:%G" /etc/unth/jobs)" = "750 root:unth" || { echo "bad jobs dir perms: $(stat -c "%a %U:%G" /etc/unth/jobs)"; exit 1; }
    grep -q "^ExecStart=/usr/bin/unth run" /usr/lib/systemd/system/unth@.service || { echo "unit does not use /usr/bin/unth"; exit 1; }
    test -f /usr/lib/systemd/system/unth@.timer
    test -f /usr/share/licenses/unthrecover/LICENSE
    /usr/bin/unth version
    su -s /bin/sh unth -c "unth --data-dir /var/lib/unthrecover backup list" >/dev/null
    rpm -e unthrecover
    test -d /var/lib/unthrecover || { echo "removal deleted backups dir"; exit 1; }
    test ! -e /usr/bin/unth
    echo "RPM TEST: PASS ('"$IMAGE"')"
  '
