#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
DEB=$(ls build/*.deb 2>/dev/null | head -1 || true)
[ -z "$DEB" ] && { echo "Нет .deb в build/"; exit 1; }
if [ -z "${GPG_PRIVATE_KEY:-}" ]; then
    echo "GPG_PRIVATE_KEY не задан — пропускаю подпись"
    exit 0
fi
if command -v dpkg-sig >/dev/null 2>&1; then
    dpkg-sig --sign builder "$DEB"
    dpkg-sig --verify "$DEB"
else
    echo "dpkg-sig не установлен — пропуск"
fi
