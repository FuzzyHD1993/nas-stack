#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v iscc >/dev/null 2>&1; then
    echo "Inno Setup (iscc) не найден. Windows: winget install JRSoftware.InnoSetup"
    exit 1
fi
iscc packaging/windows/nas-stack.iss
