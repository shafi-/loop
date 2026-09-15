#!/bin/sh
# loop installer — fetches the latest release binary for your platform.
#
#   curl -fsSL https://raw.githubusercontent.com/nerddevsltd/loop/main/install.sh | sh
#
# Install location: $INSTALL_DIR, else /usr/local/bin when writable,
# else ~/.local/bin (added to PATH hints printed at the end).
set -eu

REPO="nerddevsltd/loop"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "loop installer: unsupported OS '$(uname -s)' (darwin/linux only)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "loop installer: unsupported architecture '$(uname -m)'" >&2; exit 1 ;;
esac

command -v curl >/dev/null || { echo "loop installer: curl is required" >&2; exit 1; }

tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
  sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
[ -n "$tag" ] || { echo "loop installer: could not determine the latest release" >&2; exit 1; }

asset="loop_${tag}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$tag/$asset"
echo "installing loop $tag ($os/$arch)…"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$url" | tar -xzf - -C "$tmp"
[ -f "$tmp/loop" ] || { echo "loop installer: download did not contain the loop binary" >&2; exit 1; }

# Verify against the release checksums when they are reachable.
sums=$(curl -fsSL "https://github.com/$REPO/releases/download/$tag/SHA256SUMS" 2>/dev/null || true)
if [ -n "$sums" ]; then
  expected=$(printf '%s\n' "$sums" | grep " $asset\$" | cut -d' ' -f1)
  if [ -n "$expected" ]; then
    if command -v sha256sum >/dev/null; then
      actual=$(sha256sum "$tmp/loop" | cut -d' ' -f1)
    else
      actual=$(shasum -a 256 "$tmp/loop" | cut -d' ' -f1)
    fi
    [ "$actual" = "$expected" ] || { echo "loop installer: checksum mismatch for $asset" >&2; exit 1; }
    echo "checksum verified"
  fi
fi

if [ -n "${INSTALL_DIR:-}" ]; then
  dir=$INSTALL_DIR
elif [ -w /usr/local/bin ] 2>/dev/null; then
  dir=/usr/local/bin
else
  dir=$HOME/.local/bin
  mkdir -p "$dir"
fi
mkdir -p "$dir"
mv "$tmp/loop" "$dir/loop"
chmod +x "$dir/loop"

echo
if echo "$PATH" | tr ':' '\n' | grep -qx "$dir"; then
  echo "✓ loop $tag installed to $dir"
else
  echo "✓ loop $tag installed to $dir"
  echo "  add it to your PATH:  export PATH=\"$dir:\$PATH\""
fi
"$dir/loop" --version 2>/dev/null || true
