#!/bin/sh
# Installs the official rclone release binary, pinned by version and SHA-256
# (same pins as the Dockerfile). Used by CI; also usable on a server:
#   sudo sh scripts/install-rclone.sh [/usr/local/bin]
set -eu
VERSION=1.75.2
DEST="${1:-/usr/local/bin}"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64; SUM=349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702 ;;
  aarch64|arm64) ARCH=arm64; SUM=7e1e8d69654941b7b7df84ee74c5f7fb09cce7d5496947fb7bf16b55ff427d10 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
ZIP="rclone-v${VERSION}-linux-${ARCH}.zip"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
curl -fsSL -o "$TMP/$ZIP" "https://github.com/rclone/rclone/releases/download/v${VERSION}/${ZIP}"
echo "$SUM  $TMP/$ZIP" | sha256sum -c - >/dev/null
unzip -q -d "$TMP" "$TMP/$ZIP"
install -m 0755 "$TMP/rclone-v${VERSION}-linux-${ARCH}/rclone" "$DEST/rclone"
"$DEST/rclone" version | head -1
