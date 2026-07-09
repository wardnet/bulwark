#!/bin/sh
# install.sh — download and install the bulwark CLI from GitHub releases.
#
# Usage:
#   curl -fsSL https://github.com/wardnet/bulwark/releases/latest/download/install.sh | sh
#
# Environment:
#   BULWARK_VERSION      version to install, e.g. 1.9.0 or v1.9.0 (default: latest)
#   BULWARK_INSTALL_DIR  install directory (default: $HOME/.local/bin)
set -eu

REPO_URL="https://github.com/wardnet/bulwark"
INSTALL_DIR="${BULWARK_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${BULWARK_VERSION:-latest}"

if [ "$VERSION" = "latest" ]; then
  # The releases/latest URL redirects to .../releases/tag/v<version>.
  LOCATION=$(curl -fsSI -o /dev/null -w '%{redirect_url}' "$REPO_URL/releases/latest")
  VERSION="${LOCATION##*/}"
fi
VERSION="${VERSION#v}"
if [ -z "$VERSION" ]; then
  echo "error: could not resolve bulwark version" >&2
  exit 1
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *)
    echo "error: unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

ASSET="bulwark_${VERSION}_${OS}_${ARCH}"
# Stage inside the install dir so the final rename is atomic and never
# truncates a running binary (mv across filesystems degrades to copy+truncate).
mkdir -p "$INSTALL_DIR"
STAGE=$(mktemp -d "$INSTALL_DIR/.bulwark-install.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT

echo "downloading bulwark v${VERSION} (${OS}/${ARCH})..."
curl -fsSL -o "$STAGE/bulwark" "$REPO_URL/releases/download/v${VERSION}/${ASSET}"
curl -fsSL -o "$STAGE/checksums.txt" "$REPO_URL/releases/download/v${VERSION}/checksums.txt"

WANT=$(awk -v asset="$ASSET" '$2 == asset { print $1 }' "$STAGE/checksums.txt")
if [ -z "$WANT" ]; then
  echo "error: no checksum for $ASSET in checksums.txt" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$STAGE/bulwark" | awk '{ print $1 }')
else
  GOT=$(shasum -a 256 "$STAGE/bulwark" | awk '{ print $1 }')
fi
if [ "$GOT" != "$WANT" ]; then
  echo "error: checksum mismatch for $ASSET: got $GOT, want $WANT" >&2
  exit 1
fi

chmod 0755 "$STAGE/bulwark"
mv "$STAGE/bulwark" "$INSTALL_DIR/bulwark"
echo "installed bulwark v${VERSION} to $INSTALL_DIR/bulwark"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "note: $INSTALL_DIR is not on your PATH" ;;
esac
