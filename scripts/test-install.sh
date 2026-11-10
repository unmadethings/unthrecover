#!/bin/sh
# Tests scripts/install.sh against locally built release artifacts (file://).
set -eu
cd "$(dirname "$0")/.."
VERSION=v0.0.0-installtest
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
pass=0
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "PASS: $*"; pass=$((pass + 1)); }

make -s dist VERSION="$VERSION" >/dev/null
mkdir -p "$W/rel/$VERSION" "$W/bin"
cp dist/* "$W/rel/$VERSION/"
BASE="file://$W/rel"

# 1. happy path
sh scripts/install.sh --version "$VERSION" --dir "$W/bin" --base-url "$BASE" >/dev/null
"$W/bin/unth" version | grep -q "$VERSION" || fail "installed binary has wrong version"
ok "installs and verifies"

# 2. dry run installs nothing
rm -f "$W/bin/unth"
sh scripts/install.sh --version "$VERSION" --dir "$W/bin" --base-url "$BASE" --dry-run >/dev/null
[ ! -e "$W/bin/unth" ] || fail "dry run installed a binary"
ok "dry run"

ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
F="unth_${VERSION}_linux_${ARCH}"

# 3. tampered binary -> refused, nothing installed
cp "$W/rel/$VERSION/$F" "$W/orig"
printf 'x' >> "$W/rel/$VERSION/$F"
if sh scripts/install.sh --version "$VERSION" --dir "$W/bin" --base-url "$BASE" >"$W/out" 2>&1; then fail "tampered binary accepted"; fi
grep -q "checksum mismatch" "$W/out" || fail "no checksum mismatch message"
[ ! -e "$W/bin/unth" ] || fail "tampered binary installed"
ok "rejects tampered binary"
cp "$W/orig" "$W/rel/$VERSION/$F"

# 4. checksum entry missing -> refused
grep -v "$F" "$W/rel/$VERSION/SHA256SUMS" > "$W/sums" || true
cp "$W/rel/$VERSION/SHA256SUMS" "$W/sums.orig"
cp "$W/sums" "$W/rel/$VERSION/SHA256SUMS"
if sh scripts/install.sh --version "$VERSION" --dir "$W/bin" --base-url "$BASE" >"$W/out" 2>&1; then fail "missing checksum accepted"; fi
grep -q "no entry" "$W/out" || fail "no missing-entry message"
ok "rejects missing checksum entry"
cp "$W/sums.orig" "$W/rel/$VERSION/SHA256SUMS"

# 5. invalid version / http downgrade refused
if sh scripts/install.sh --version 'v1;rm -rf /' --dir "$W/bin" --base-url "$BASE" >/dev/null 2>&1; then fail "invalid version accepted"; fi
ok "rejects invalid version"
if sh scripts/install.sh --version "$VERSION" --dir "$W/bin" --base-url "http://example.invalid" >/dev/null 2>&1; then fail "plain http accepted"; fi
ok "refuses a plain-http base URL"

echo "install tests: $pass passed"
