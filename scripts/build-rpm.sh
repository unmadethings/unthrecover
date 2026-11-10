#!/bin/sh
# Builds an RPM package (Fedora, RHEL, Rocky, AlmaLinux, openSUSE) from a release
# binary in dist/, with the same layout and behaviour as the .deb.
#   scripts/build-rpm.sh <version-tag> <amd64|arm64>
# Output: dist/unthrecover_<tag without v>_<arch>.rpm (no "~" in the file name; the
# RPM version is 0.3.0~beta so it sorts before 0.3.0).
# Uses nfpm (pure Go, pinned) so no rpmbuild is needed on the build machine.
set -eu
TAG="$1"
ARCH="$2"
NFPM_VERSION=v2.47.0
cd "$(dirname "$0")/.."
BIN="dist/unth_${TAG}_linux_${ARCH}"
[ -f "$BIN" ] || { echo "missing $BIN (run make dist first)" >&2; exit 1; }
case "$TAG" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "invalid tag $TAG" >&2; exit 1 ;; esac

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
sed 's#/usr/local/bin/unth#/usr/bin/unth#' deployments/systemd/unth@.service > "$STAGE/unth@.service"

# The user exists before files are laid down, so /etc/unth/jobs is owned by
# group unth in the package itself (rpm -V stays clean).
cat > "$STAGE/preinstall.sh" <<'EOF'
#!/bin/sh
set -e
if ! getent passwd unth >/dev/null; then
  useradd --system --user-group --home-dir /var/lib/unthrecover --no-create-home \
    --shell /sbin/nologin --comment "Unth Recover" unth
fi
exit 0
EOF
cat > "$STAGE/postinstall.sh" <<'EOF'
#!/bin/sh
set -e
install -d -m 0700 -o unth -g unth /var/lib/unthrecover
if getent group docker >/dev/null; then usermod -aG docker unth || true; fi
if [ -d /run/systemd/system ]; then systemctl daemon-reload || true; fi
echo "unthrecover: put job configs in /etc/unth/jobs/<job>.yaml (secrets in <job>.env, 0600),"
echo "             then: systemctl enable --now unth@<job>.timer   (see /usr/share/doc/unthrecover)"
exit 0
EOF
cat > "$STAGE/postremove.sh" <<'EOF'
#!/bin/sh
# Backups in /var/lib/unthrecover and configs in /etc/unth are never removed automatically.
if [ -d /run/systemd/system ]; then systemctl daemon-reload || true; fi
exit 0
EOF

cat > "$STAGE/nfpm.yaml" <<EOF
name: unthrecover
arch: ${ARCH}
platform: linux
version: ${TAG#v}
version_schema: semver
release: 1
section: admin
maintainer: Unmade Things <noreply@github.com>
vendor: Unmade Things
homepage: https://github.com/unmadethings/unthrecover
license: Apache-2.0
description: |
  Backups with proven recovery (PostgreSQL, Docker volumes).
  Unth Recover backs up PostgreSQL databases and Docker volumes and proves each
  backup can be recovered by restoring it into a fresh isolated container and
  comparing data, schema and files with the source. Pre-release software.
recommends:
  - postgresql
  - docker
contents:
  - src: ${PWD}/${BIN}
    dst: /usr/bin/unth
    file_info: {mode: 0755}
  - src: ${STAGE}/unth@.service
    dst: /usr/lib/systemd/system/unth@.service
    file_info: {mode: 0644}
  - src: ${PWD}/deployments/systemd/unth@.timer
    dst: /usr/lib/systemd/system/unth@.timer
    file_info: {mode: 0644}
  - dst: /etc/unth
    type: dir
    file_info: {mode: 0755}
  - dst: /etc/unth/jobs
    type: dir
    file_info: {mode: 0750, owner: root, group: unth}
  - src: ${PWD}/examples/backup.yaml
    dst: /usr/share/doc/unthrecover/examples/backup.yaml
    type: doc
  - src: ${PWD}/examples/volume.yaml
    dst: /usr/share/doc/unthrecover/examples/volume.yaml
    type: doc
  - src: ${PWD}/deployments/cron/unthrecover.cron
    dst: /usr/share/doc/unthrecover/examples/unthrecover.cron
    type: doc
  - src: ${PWD}/README.md
    dst: /usr/share/doc/unthrecover/README.md
    type: doc
  - src: ${PWD}/CHANGELOG.md
    dst: /usr/share/doc/unthrecover/CHANGELOG.md
    type: doc
  - src: ${PWD}/LICENSE
    dst: /usr/share/licenses/unthrecover/LICENSE
    type: license
scripts:
  preinstall: ${STAGE}/preinstall.sh
  postinstall: ${STAGE}/postinstall.sh
  postremove: ${STAGE}/postremove.sh
EOF

OUT="dist/unthrecover_${TAG#v}_${ARCH}.rpm"
go run "github.com/goreleaser/nfpm/v2/cmd/nfpm@${NFPM_VERSION}" package \
  --config "$STAGE/nfpm.yaml" --packager rpm --target "$OUT" >/dev/null
echo "$OUT"
