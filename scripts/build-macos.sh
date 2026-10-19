#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="1.0.0-beta"; STAGE="build/dmg-stage"
rm -rf build && mkdir -p "$STAGE"
cp -r scripts docs README.md LICENSE "$STAGE/"
cat > "$STAGE/README.txt" <<'TXT'
NAS Stack
1. Откройте Терминал, перейдите в эту папку
2. bash scripts/install.sh
3. http://localhost:8090/setup
Если macOS блокирует: xattr -d com.apple.quarantine scripts/install.sh
TXT
if command -v hdiutil >/dev/null 2>&1; then
    hdiutil create -volname "NAS Stack" -srcfolder "$STAGE" -ov -format UDZO "build/nas-stack-${VERSION}.dmg"
    echo "OK: build/nas-stack-${VERSION}.dmg"
else
    echo "hdiutil доступен только на macOS — staging в $STAGE"
fi
