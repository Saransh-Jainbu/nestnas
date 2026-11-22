#!/bin/sh
# NestNAS installer for Linux and macOS.
#   curl -fsSL https://raw.githubusercontent.com/Saransh-Jainbu/nestnas/main/install.sh | sh
# Options (env): NESTNAS_REPO=owner/repo  NESTNAS_VERSION=v0.1.0  NESTNAS_BINARY=/path/to/local/binary
#                NESTNAS_PORT=8080  NESTNAS_NO_SERVICE=1  NESTNAS_BIN_DIR=~/.local/bin
set -eu

REPO=${NESTNAS_REPO:-Saransh-Jainbu/nestnas}
VERSION=${NESTNAS_VERSION:-latest}
BIN_DIR=${NESTNAS_BIN_DIR:-$HOME/.local/bin}
PORT=${NESTNAS_PORT:-8080}

say() { printf '  %s\n' "$*"; }
die() { printf '\n  Error: %s\n\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] && [ -z "${SUDO_USER:-}" ] && say "Note: running as root. NestNAS is meant to run as your normal user; consider re-running without sudo."

mkdir -p "$BIN_DIR"
DEST="$BIN_DIR/nestnas"

if [ -n "${NESTNAS_BINARY:-}" ]; then
  [ -f "$NESTNAS_BINARY" ] || die "NESTNAS_BINARY not found: $NESTNAS_BINARY"
  say "Installing from local file $NESTNAS_BINARY"
  cp "$NESTNAS_BINARY" "$DEST.new"
else
  case "$(uname -s)" in Linux) os=linux ;; Darwin) os=macos ;; *) die "Unsupported OS: $(uname -s). On Windows use install.ps1." ;; esac
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "Unsupported CPU: $(uname -m)" ;; esac
  asset="nestnas-$os-$arch"
  if [ "$VERSION" = latest ]; then base="https://github.com/$REPO/releases/latest/download"
  else base="https://github.com/$REPO/releases/download/$VERSION"; fi

  if command -v curl >/dev/null 2>&1; then dl() { curl -fsSL "$1" -o "$2"; }
  elif command -v wget >/dev/null 2>&1; then dl() { wget -qO "$2" "$1"; }
  else die "Need curl or wget to download NestNAS."; fi

  tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
  say "Downloading $asset ($VERSION)..."
  dl "$base/$asset" "$tmp/$asset" || die "Download failed. Check your internet connection, or that a release exists for $os/$arch."
  dl "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "Couldn't download checksums; refusing to install an unverified binary."
  want=$(grep " $asset\$" "$tmp/SHA256SUMS" | awk '{print $1}')
  [ -n "$want" ] || die "No checksum listed for $asset."
  if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
  else got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}'); fi
  [ "$got" = "$want" ] || die "Checksum mismatch! The download is corrupted or tampered with. Nothing was installed."
  say "Checksum verified."
  cp "$tmp/$asset" "$DEST.new"
fi

chmod +x "$DEST.new"
"$DEST.new" version >/dev/null 2>&1 || { rm -f "$DEST.new"; die "The downloaded program doesn't run on this machine."; }
mv "$DEST.new" "$DEST"
say "Installed to $DEST"

case ":$PATH:" in *":$BIN_DIR:"*) ;; *) say "Tip: add $BIN_DIR to your PATH:  echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.profile" ;; esac

if [ "${NESTNAS_NO_SERVICE:-}" = 1 ]; then
  say "Skipped autostart. Start NestNAS any time with: $DEST"
else
  "$DEST" service install --port "$PORT"
fi
