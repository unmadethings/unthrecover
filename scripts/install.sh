#!/bin/sh
# Unth Recover installer.
#
# Downloads a release binary and SHA256SUMS, verifies the checksum and
# installs the binary. Nothing is installed if verification fails.
#
#   sh install.sh [--version vX.Y.Z[-pre]] [--dir /usr/local/bin] [--base-url URL] [--dry-run]
#
# --version   release tag (default: latest published release)
# --dir       install directory (default: /usr/local/bin; must be writable — use sudo)
# --base-url  download base (default: https://github.com/unmadethings/unthrecover/releases/download)
#             the files are fetched from <base-url>/<version>/<file>; file:// works for mirrors/tests
#
# NOTE: SHA-256 protects against corrupted or swapped downloads only if
# SHA256SUMS itself is trusted. Both come from the same release; signed
# checksums are planned. Compare SHA256SUMS with the release page if in doubt.
set -eu

REPO="unmadethings/unthrecover"
VERSION=""
DIR="/usr/local/bin"
BASE_URL="https://github.com/${REPO}/releases/download"
DRY_RUN=0

die() { echo "install.sh: error: $*" >&2; exit 1; }
say() { echo "install.sh: $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --dir) [ $# -ge 2 ] || die "--dir needs a value"; DIR="$2"; shift 2 ;;
    --base-url) [ $# -ge 2 ] || die "--base-url needs a value"; BASE_URL="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

command -v curl >/dev/null 2>&1 || die "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "sha256sum or shasum is required"
fi

[ "$(uname -s)" = "Linux" ] || die "only Linux is supported in this release"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) \
    || die "cannot determine the latest release (use --version)"
  [ -n "$VERSION" ] || die "no published stable release found; pre-releases need --version"
fi
case "$VERSION" in
  v[0-9]*) ;;
  *) die "invalid version: $VERSION" ;;
esac
if printf '%s' "$VERSION" | grep -q '[^A-Za-z0-9.+-]'; then die "invalid version: $VERSION"; fi

FILE="unth_${VERSION}_linux_${ARCH}"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

say "downloading ${FILE} and SHA256SUMS (${VERSION})"
curl -fsSL --proto '=https,file' -o "$TMP/$FILE" "${BASE_URL}/${VERSION}/${FILE}" || die "download failed: ${FILE}"
curl -fsSL --proto '=https,file' -o "$TMP/SHA256SUMS" "${BASE_URL}/${VERSION}/SHA256SUMS" || die "download failed: SHA256SUMS"

EXPECTED=$(awk -v f="$FILE" '$2 == f || $2 == "*"f {print $1}' "$TMP/SHA256SUMS")
[ -n "$EXPECTED" ] || die "SHA256SUMS has no entry for ${FILE}"
[ "$(printf '%s\n' "$EXPECTED" | wc -l)" -eq 1 ] || die "SHA256SUMS has several entries for ${FILE}"
ACTUAL=$(sha256 "$TMP/$FILE")
[ "$EXPECTED" = "$ACTUAL" ] || die "checksum mismatch for ${FILE}: expected ${EXPECTED}, got ${ACTUAL} — NOT installed"
say "checksum OK (${ACTUAL})"

chmod 0755 "$TMP/$FILE"
"$TMP/$FILE" version >/dev/null 2>&1 || die "downloaded binary does not run on this system"

if [ "$DRY_RUN" -eq 1 ]; then
  say "dry run: would install to ${DIR}/unth"
  exit 0
fi
[ -d "$DIR" ] || die "install directory does not exist: $DIR"
[ -w "$DIR" ] || die "cannot write to $DIR (re-run with sudo or use --dir)"
# Atomic replace: copy next to the target, then rename.
cp "$TMP/$FILE" "$DIR/.unth.new.$$"
chmod 0755 "$DIR/.unth.new.$$"
mv -f "$DIR/.unth.new.$$" "$DIR/unth"
say "installed $("$DIR/unth" version)"
say "next: unth doctor"
