#!/usr/bin/env bash
# Build a Debian/Ubuntu package of NestNAS Desktop:  ./package-deb.sh [version]
# Needs: go, libgtk-3-dev, libwebkit2gtk-4.0-dev, dpkg-deb
set -euo pipefail
cd "$(dirname "$0")"
VER=${1:-0.1.0}
ROOT=$(mktemp -d); trap 'rm -rf "$ROOT"' EXIT
mkdir -p "$ROOT/usr/bin" "$ROOT/usr/share/applications" "$ROOT/usr/share/icons/hicolor/256x256/apps" "$ROOT/DEBIAN" ../dist

go build -tags production -ldflags "-s -w -X main.version=$VER" -o "$ROOT/usr/bin/nestnas-desktop" .
install -m 644 icon.png "$ROOT/usr/share/icons/hicolor/256x256/apps/nestnas.png"
cat > "$ROOT/usr/share/applications/nestnas.desktop" <<DESK
[Desktop Entry]
Type=Application
Name=NestNAS
Comment=Turn this computer into your own cloud storage
Exec=nestnas-desktop
Icon=nestnas
Terminal=false
Categories=Network;FileTransfer;Utility;
DESK
cat > "$ROOT/DEBIAN/control" <<CTRL
Package: nestnas
Version: $VER
Section: net
Priority: optional
Architecture: $(dpkg --print-architecture)
Depends: libgtk-3-0, libwebkit2gtk-4.0-37
Maintainer: NestNAS contributors <noreply@example.com>
Description: Turn any laptop into your own NAS / personal cloud
 NestNAS gives you a web file manager, per-user quotas, share links,
 network drives and one-click apps, in a simple desktop app.
CTRL
find "$ROOT" -type d -exec chmod 755 {} +   # mktemp is 0700; package dirs must be 0755
dpkg-deb --build --root-owner-group "$ROOT" "../dist/nestnas_${VER}_$(dpkg --print-architecture).deb"
echo "Built dist/nestnas_${VER}_$(dpkg --print-architecture).deb"
