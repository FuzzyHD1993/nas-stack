#!/usr/bin/env bash
# update.sh — обновление стека с авто-откатом.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"; cd "$REPO_DIR"

GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'; NC='\033[0m'
log() { echo -e "${CYAN}▶${NC} $*"; }
ok()  { echo -e "${GREEN}✅${NC} $*"; }
err() { echo -e "${RED}❌${NC} $*"; }

if command -v podman >/dev/null 2>&1; then COMPOSE_CMD="podman compose"; else COMPOSE_CMD="docker compose"; fi

BEFORE=$(git rev-parse HEAD); log "Before: $BEFORE"
if command -v snapper >/dev/null 2>&1; then
    sudo -n snapper -c root create --description "nas-stack update $(date +%Y%m%d-%H%M%S)" 2>/dev/null || true
fi
log "git pull..."; git pull || { err "pull failed"; exit 1; }
log "build..."; $COMPOSE_CMD build || { err "build failed"; git reset --hard "$BEFORE"; exit 1; }
log "up..."; $COMPOSE_CMD up -d || { err "up failed"; git reset --hard "$BEFORE"; exit 1; }
# gateway/dashboard кэшируют IP под-сервисов после пересоздания
log "restart gateway/dashboard (IP cache)..."
$COMPOSE_CMD restart gateway || true
sleep 2
$COMPOSE_CMD restart web-dashboard || true
sleep 30
if bash scripts/smoke-test.sh; then
    ok "Обновление успешно"; git log --oneline -1
else
    err "Smoke failed — откат"; git reset --hard "$BEFORE"; $COMPOSE_CMD build; $COMPOSE_CMD up -d
    $COMPOSE_CMD restart gateway || true; sleep 2; $COMPOSE_CMD restart web-dashboard || true; exit 1
fi
