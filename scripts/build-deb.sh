#!/bin/sh
# Builds a Debian/Ubuntu package from a release binary in dist/.
#   scripts/build-deb.sh <version-tag> <amd64|arm64>
# Output: dist/unthrecover_<debversion>_<arch>.deb
set -eu
TAG="$1"
ARCH="$2"
cd "$(dirname "$0")/.."
BIN="dist/unth_${TAG}_linux_${ARCH}"
[ -f "$BIN" ] || { echo "missing $BIN (run make dist first)" >&2; exit 1; }
# v0.2.0-alpha -> 0.2.0~alpha (sorts before 0.2.0 in dpkg)
DEBVER=$(printf '%s' "${TAG#v}" | sed 's/-/~/')
ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
chmod 0755 "$ROOT"

install -d -m 0755 "$ROOT/DEBIAN" "$ROOT/usr/bin" "$ROOT/lib/systemd/system" \
  "$ROOT/usr/share/doc/unthrecover/examples" "$ROOT/etc/unth/jobs"
install -m 0755 "$BIN" "$ROOT/usr/bin/unth"
sed 's#/usr/local/bin/unth#/usr/bin/unth#' deployments/systemd/unth@.service > "$ROOT/lib/systemd/system/unth@.service"
install -m 0644 deployments/systemd/unth@.timer "$ROOT/lib/systemd/system/unth@.timer"
chmod 0644 "$ROOT/lib/systemd/system/unth@.service"
install -m 0644 examples/backup.yaml examples/volume.yaml "$ROOT/usr/share/doc/unthrecover/examples/"
install -m 0644 deployments/cron/unthrecover.cron "$ROOT/usr/share/doc/unthrecover/examples/"
install -m 0644 README.md CHANGELOG.md "$ROOT/usr/share/doc/unthrecover/"
install -m 0644 LICENSE "$ROOT/usr/share/doc/unthrecover/copyright"

SIZE=$(du -sk "$ROOT" | cut -f1)
cat > "$ROOT/DEBIAN/control" <<EOF
Package: unthrecover
Version: ${DEBVER}
Section: admin
Priority: optional
Architecture: ${ARCH}
Installed-Size: ${SIZE}
Maintainer: Unmade Things <noreply@github.com>
Homepage: https://github.com/unmadethings/unthrecover
Depends: postgresql-client
Recommends: docker.io | docker-ce
Description: Backups with proven recovery (PostgreSQL, Docker volumes)
 Unth Recover backs up PostgreSQL databases and Docker volumes and proves each
 backup can be recovered by restoring it into a fresh isolated container and
 comparing data, schema and files with the source. Pre-release software.
EOF
cat > "$ROOT/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if [ "$1" = "configure" ]; then
  if ! getent passwd unth >/dev/null; then
    adduser --system --group --home /var/lib/unthrecover --no-create-home --shell /usr/sbin/nologin unth >/dev/null
  fi
  install -d -m 0700 -o unth -g unth /var/lib/unthrecover
  chown root:unth /etc/unth/jobs && chmod 0750 /etc/unth/jobs
  if getent group docker >/dev/null; then
    usermod -aG docker unth || true
  fi
  if [ -d /run/systemd/system ]; then systemctl daemon-reload || true; fi
  echo "unthrecover: put job configs in /etc/unth/jobs/<job>.yaml (secrets in <job>.env, 0600),"
  echo "             then: systemctl enable --now unth@<job>.timer   (see /usr/share/doc/unthrecover)"
fi
exit 0
EOF
cat > "$ROOT/DEBIAN/postrm" <<'EOF'
#!/bin/sh
set -e
if [ -d /run/systemd/system ]; then systemctl daemon-reload || true; fi
# Backups in /var/lib/unthrecover and configs in /etc/unth are never removed automatically.
exit 0
EOF
chmod 0755 "$ROOT/DEBIAN/postinst" "$ROOT/DEBIAN/postrm"
OUT="dist/unthrecover_${DEBVER}_${ARCH}.deb"
dpkg-deb --root-owner-group --build "$ROOT" "$OUT" >/dev/null
echo "$OUT"
