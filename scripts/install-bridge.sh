#!/usr/bin/env bash
# Install the Cursor SDK bridge that pig-cursor-sdk drives.
#
#   scripts/install-bridge.sh            # latest release for this machine
#   scripts/install-bridge.sh v1.2.0     # a specific sdk-bridge tag
#
# Downloads cursor-sdk-bridge-standalone-<os>-<arch>.tar.gz from
# https://github.com/cursor/sdk-bridge/releases/latest, verifies its SHA-256
# against the release's SHA256SUMS.txt, and installs the executable where the
# extension looks for it:
#
#   ~/.pig/agent/pig-cursor-sdk/bin/cursor-sdk-bridge
#
# Override the destination with CURSOR_SDK_BRIDGE_BIN (an existing file or
# directory anywhere) — the extension honours that variable first.
set -euo pipefail

TAG="${1:-latest}"
REPO="cursor/sdk-bridge"
DEST_DIR="${PIG_HOME:-$HOME/.pig}/agent/pig-cursor-sdk/bin"
if [ -n "${CURSOR_SDK_BRIDGE_BIN:-}" ]; then
  if [ -d "$CURSOR_SDK_BRIDGE_BIN" ]; then DEST_DIR="$CURSOR_SDK_BRIDGE_BIN"; else DEST_DIR="$(dirname "$CURSOR_SDK_BRIDGE_BIN")"; fi
fi

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  MINGW*|MSYS*|CYGWIN*) OS=win32 ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=x64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
if [ "$OS" = "win32" ] && [ "$ARCH" != "x64" ]; then
  echo "win32 bridge archives are x64 only" >&2; exit 1
fi
ASSET="cursor-sdk-bridge-standalone-${OS}-${ARCH}.tar.gz"
BIN=cursor-sdk-bridge
if [ "$OS" = win32 ]; then BIN="$BIN.exe"; fi

api="https://api.github.com/repos/$REPO/releases"
[ "$TAG" = latest ] && api="$api/latest" || api="$api/tags/$TAG"
echo "release:  $([ "$TAG" = latest ] && echo latest || echo "$TAG")  platform: ${OS}-${ARCH}"
command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }
command -v shasum >/dev/null || command -v sha256sum >/dev/null || { echo "need shasum or sha256sum" >&2; exit 1; }

sha_of() { # <file>
  if command -v shasum >/dev/null; then shasum -a 256 "$1" | cut -d' ' -f1
  else sha256sum "$1" | cut -d' ' -f1; fi
}

json=$(curl -fsSL -H 'Accept: application/vnd.github+json' "$api")
url=$(printf '%s' "$json" | python3 -c "
import json,sys
asset=sys.argv[1]
rel=json.load(sys.stdin)
for a in rel['assets']:
    if a['name']==asset: print(a['browser_download_url']); break
else:
    sys.exit('asset %s not found in release %s' % (asset, rel.get('tag_name')))
" "$ASSET")
sums_url=$(printf '%s' "$json" | python3 -c "
import json,sys
rel=json.load(sys.stdin)
for a in rel['assets']:
    if a['name']=='SHA256SUMS.txt': print(a['browser_download_url']); break
")
tag=$(printf '%s' "$json" | python3 -c "import json,sys;print(json.load(sys.stdin)['tag_name'])")
echo "tag:      $tag"
echo "asset:    $url"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fL --progress-bar -o "$tmp/$ASSET" "$url"
[ -n "$sums_url" ] && curl -fsSL -o "$tmp/SHA256SUMS.txt" "$sums_url"

if [ -f "$tmp/SHA256SUMS.txt" ]; then
  want=$(awk -v f="$ASSET" '$2==f || $2=="./"f {print $1; exit}' "$tmp/SHA256SUMS.txt")
  have=$(sha_of "$tmp/$ASSET")
  if [ -n "$want" ]; then
    if [ "$want" != "$have" ]; then
      echo "checksum mismatch: expected $want got $have" >&2; exit 1
    fi
    echo "sha256:   verified $have"
  else
    echo "sha256:   $ASSET not listed in SHA256SUMS.txt; refusing to skip verification" >&2; exit 1
  fi
else
  echo "sha256:   no SHA256SUMS.txt published for this release; refusing an unverified binary" >&2
  exit 1
fi

tar -xzf "$tmp/$ASSET" -C "$tmp"
src=$(find "$tmp" -type f -name "$BIN" -print -quit)
[ -n "$src" ] || { echo "executable $BIN not found in the archive" >&2; exit 1; }
mkdir -p "$DEST_DIR"
# Replace atomically: a running extension may hold the old inode.
mv -f "$src" "$DEST_DIR/$BIN.tmp"
chmod +x "$DEST_DIR/$BIN.tmp"
mv -f "$DEST_DIR/$BIN.tmp" "$DEST_DIR/$BIN"
echo "installed: $DEST_DIR/$BIN"
echo
echo "Now start pig and run /cursor-login (a key from cursor.com/dashboard -> API Keys),"
echo "then: pig --model cursor/grok-4.7 -p \"hello\""
