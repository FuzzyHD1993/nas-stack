#!/usr/bin/env bash
# uninstall.sh — остановка и удаление стека.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"; cd "$REPO_DIR"
GREEN='\033[0;32m'; CYAN='\033[0;36m'; NC='\033[0m'
log() { echo -e "${CYAN}▶${NC} $*"; }
ok()  { echo -e "${GREEN}✅${NC} $*"; }
if command -v podman >/dev/null 2>&1; then COMPOSE_CMD="podman compose"; else COMPOSE_CMD="docker compose"; fi
log "Останавливаю стек..."; $COMPOSE_CMD down -v
log "Удаляю systemd-юниты..."
for unit in nas-stack nas-notify nas-stack-watchdog; do
    systemctl --user stop "$unit.service" "$unit.timer" 2>/dev/null || true
    systemctl --user disable "$unit.service" "$unit.timer" 2>/dev/null || true
    rm -f ~/.config/systemd/user/"$unit".service ~/.config/systemd/user/"$unit".timer
done
systemctl --user daemon-reload 2>/dev/null || true
read -r -p "⚠ Удалить .env? [y/N] " REPLY; echo ""
if [[ "$REPLY" =~ ^[Yy]$ ]]; then rm -f .env; ok ".env удалён"; fi
ok "Удаление завершено"
