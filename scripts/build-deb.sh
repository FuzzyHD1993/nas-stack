#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="1.0.0-beta"; ARCH="all"
PKG="build/nas-stack_${VERSION}_${ARCH}"
rm -rf build && mkdir -p "$PKG"
cp -r packaging/linux/debian/* "$PKG/"
mkdir -p "$PKG/usr/share/nas-stack"
cp -r scripts systemd docs "$PKG/usr/share/nas-stack/"
cp README.md LICENSE .env.example "$PKG/usr/share/nas-stack/"
mkdir -p "$PKG/usr/local/bin"
printf '#!/bin/bash\nexec /usr/share/nas-stack/scripts/install.sh "$@"\n' > "$PKG/usr/local/bin/nas-stack-install"
chmod +x "$PKG/usr/local/bin/nas-stack-install"
if command -v dpkg-deb >/dev/null 2>&1; then
    dpkg-deb --build "$PKG"
    echo "OK: build/nas-stack_${VERSION}_${ARCH}.deb"
else
    echo "dpkg-deb не установлен — пакет собран в $PKG (соберите на Debian/Ubuntu)"
fi
