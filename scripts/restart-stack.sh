#!/usr/bin/env bash
# restart-stack.sh — безопасный перезапуск стека на rootless Podman.
#
# Почему не `up -d --force-recreate` для всех сразу: при массовом пересоздании
# rootless Podman иногда теряет conmon (падает redis/postgres) и port-forward'ы
# (контейнер Up, но localhost:port — connection refused).
#
# Порядок: media → остальные → DNS reload → search/gateway/dashboard →
# проверка host-портов (restart/recreate) → прогрев music-index → smoke.
#
# Использование:
#   scripts/restart-stack.sh            # приложения (postgres/redis не трогаем)
#   scripts/restart-stack.sh --with-db  # + перезапуск postgres/redis
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
cd "$REPO_DIR"

WITH_DB=0
[ "${1:-}" = "--with-db" ] && WITH_DB=1

GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'; NC='\033[0m'
log() { echo -e "${CYAN}▶${NC} $*"; }
ok()  { echo -e "${GREEN}✅${NC} $*"; }
err() { echo -e "${RED}❌${NC} $*"; }

if command -v podman >/dev/null 2>&1; then PODMAN=podman; else PODMAN=docker; fi
running() { $PODMAN ps --format '{{.Names}}' | grep -qx "$1"; }
restart_one() {
    local name=$1
    if running "$name"; then
        log "restart $name"
        $PODMAN restart "$name" >/dev/null 2>&1 || err "не удалось перезапустить $name"
    fi
}
healthcheck_one() { $PODMAN healthcheck run "$1" >/dev/null 2>&1 || true; }

# 1. Инфраструктура — только по запросу.
if [ "$WITH_DB" = "1" ]; then
    restart_one nas-redis
    restart_one nas-postgres
    log "жду postgres..."
    for _ in $(seq 1 30); do
        if $PODMAN exec nas-postgres pg_isready -U nas -d nas_history >/dev/null 2>&1; then
            ok "postgres готов"; break
        fi
        if ! running nas-postgres; then
            log "postgres не поднялся — стартую заново"
            $PODMAN start nas-postgres >/dev/null 2>&1 || true
        fi
        sleep 2
    done
    sleep 2
fi

# 2. media — раньше search (search кэширует IP media-service).
restart_one nas-media
sleep 2

# 3. Остальные приложения, кроме gateway/dashboard/media/redis/postgres.
mapfile -t NAMES < <($PODMAN ps --filter label=com.docker.compose.project=nas-stack \
    --format '{{.Names}}' | grep -vE '^nas-(gateway|dashboard|media|redis|postgres)$' || true)
for c in "${NAMES[@]}"; do
    if [ -n "$c" ]; then
        restart_one "$c"
        sleep 1
    fi
done
sleep 4

# 3.1 Если conmon всё же уронил контейнер (Exited) — поднимаем его обратно.
for c in $($PODMAN ps -a --filter label=com.docker.compose.project=nas-stack --format '{{.Names}}' 2>/dev/null); do
    if ! running "$c"; then
        log "start $c (был остановлен)"
        $PODMAN start "$c" >/dev/null 2>&1 || err "не удалось стартовать $c"
    fi
done
sleep 3

# 4. Обновляем DNS (лечит старые IP после пересозданий).
log "network reload (DNS)..."
$PODMAN network reload --all >/dev/null 2>&1 || true

# 5. Search и gateway — после DNS reload, dashboard — последним.
restart_one nas-search
sleep 2
restart_one nas-gateway
sleep 3
restart_one nas-dashboard
sleep 10

# 6. Проверка host-портов. Rootless Podman иногда теряет port-forward именно
# после массового restart, поэтому проверяем в самом конце и лечим.
declare -A SVC_PORT=(
    [torrent-service]=8081 [stats-service]=8082 [scheduler-service]=8084
    [history-service]=8085 [auth-service]=8086 [books-service]=8087
    [search-service]=8088 [youtube-service]=8089 [media-service]=8091
    [music-service]=8092
)
wait_port() {
    local port=$1
    for _ in $(seq 1 8); do
        if [ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "http://localhost:$port/health" 2>/dev/null)" = "200" ]; then
            return 0
        fi
        sleep 2
    done
    return 1
}
RECREATED=0
for svc in "${!SVC_PORT[@]}"; do
    port=${SVC_PORT[$svc]}
    wait_port "$port" && continue
    c=$($PODMAN ps --filter label=com.docker.compose.service="$svc" --format '{{.Names}}' | head -1)
    [ -z "$c" ] && continue
    if running "$c"; then
        log "порт $port ($svc) не отвечает — restart"
        $PODMAN restart "$c" >/dev/null 2>&1 || true
    else
        log "порт $port ($svc) не отвечает — start"
        $PODMAN start "$c" >/dev/null 2>&1 || true
    fi
    wait_port "$port" && continue
    log "порт $port ($svc) всё ещё не отвечает — recreate"
    $PODMAN rm -f "$c" >/dev/null 2>&1 || true
    $PODMAN compose --profile flibusta-local up -d --no-build "$svc" >/dev/null 2>&1 || true
    RECREATED=1
    wait_port "$port" || err "порт $port ($svc) недоступен после recreate"
done

# Если что-то пересоздавали — IP изменился, обновляем gateway/dashboard.
if [ "$RECREATED" = "1" ]; then
    log "были recreate — обновляю gateway/dashboard"
    restart_one nas-gateway
    sleep 3
    restart_one nas-dashboard
    sleep 5
fi

# 7. Обновляем статусы healthcheck (иногда залипают после restart).
for c in $($PODMAN ps --filter label=com.docker.compose.project=nas-stack --format '{{.Names}}' 2>/dev/null); do
    healthcheck_one "$c"
done

# 8. Прогрев music-index (первый скан CIFS может занять больше 30 с).
log "прогрев music-index..."
if curl -sS -o /dev/null --max-time 150 "http://localhost:8092/playlists/smart?type=recent" 2>/dev/null; then
    ok "music-index готов"
else
    err "music-index не прогрелся (проверьте nas-music)"
fi

# 9. Проверки.
ui_code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 http://localhost:8090/ 2>/dev/null || echo 000)
if [ "$ui_code" = "200" ]; then
    ok "UI: http://localhost:8090/ — 200"
else
    err "UI вернул $ui_code"
fi

if bash scripts/smoke-test.sh; then
    ok "Перезапуск завершён"
else
    err "Smoke не прошёл — логи: podman logs --tail 100 nas-gateway nas-dashboard"
    exit 1
fi
