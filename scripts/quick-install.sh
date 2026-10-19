#!/usr/bin/env bash
# ============================================================
# NAS-Stack — 1-Click Installer
# Использование:
#   curl -fsSL https://raw.githubusercontent.com/FuzzyHD1993/nas-stack/main/scripts/quick-install.sh | bash
# ============================================================
set -euo pipefail

GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'

echo -e "${CYAN}🖥  Добро пожаловать в NAS-Stack!${NC}"
echo -e "${CYAN}   Установка займёт 2-3 минуты...${NC}\n"

if ! command -v git >/dev/null 2>&1; then
    echo "❌ Git не установлен."
    exit 1
fi
if ! command -v podman >/dev/null 2>&1 && ! command -v docker >/dev/null 2>&1; then
    echo "❌ Podman или Docker не найден."
    echo "   Linux: sudo apt install podman   |   macOS: https://docker.com"
    exit 1
fi

INSTALL_DIR="$HOME/nas-stack"
if [ -d "$INSTALL_DIR/.git" ]; then
    echo -e "⚠ Папка $INSTALL_DIR уже существует. Обновляю..."
    cd "$INSTALL_DIR" && git pull --ff-only
else
    echo -e "▶ Скачиваю NAS-Stack..."
    git clone https://github.com/FuzzyHD1993/nas-stack.git "$INSTALL_DIR"
    cd "$INSTALL_DIR"
fi

echo -e "▶ Запускаю установку..."
bash scripts/install.sh

URL="http://localhost:8090/setup"
echo -e "\n${GREEN}✅ Готово! Откройте:${NC} $URL"
if command -v xdg-open >/dev/null 2>&1; then xdg-open "$URL" >/dev/null 2>&1 || true
elif command -v open >/dev/null 2>&1; then open "$URL" >/dev/null 2>&1 || true
fi
