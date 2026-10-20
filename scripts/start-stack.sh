#!/usr/bin/env bash
# ============================================================
# start-stack.sh — надёжный запуск NAS-Stack (rootless Podman)
#
# Почему не массовый `podman compose up -d`: на rootless Podman
# иногда теряется conmon и port-forward, из-за чего часть
# контейнеров остаётся Exited, а порты — закрытыми.
#
# Скрипт поднимает сервисы по порядку, ждёт зависимости и NAS,
# пересоздаёт зависшие контейнеры и проверяет /health на всех
# портах. Повторный запуск безопасен (идемпотентен).
#
# Использование: bash scripts/start-stack.sh
# ============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
cd "$REPO_DIR"

# Не засорять вывод предупреждением podman о внешнем compose-провайдере.
export PODMAN_COMPOSE_WARNING_LOGS=false

PODMAN="${PODMAN:-podman}"

if [ -t 1 ]; then
    GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
else
    GREEN=''; RED=''; YELLOW=''; CYAN=''; NC=''
fi
log()  { echo -e "${CYAN}▶${NC} $*"; }
ok()   { echo -e "${GREEN}✅${NC} $*"; }
warn() { echo -e "${YELLOW}⚠️${NC} $*"; }
err()  { echo -e "${RED}❌${NC} $*"; }

# ------------------------------------------------------------
# Проверки окружения
# ------------------------------------------------------------
if ! command -v "$PODMAN" >/dev/null 2>&1; then
    err "podman не найден в PATH"
    exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
    err "curl не найден в PATH"
    exit 1
fi
if ! "$PODMAN" compose version >/dev/null 2>&1; then
    err "podman compose недоступен (нужен compose-провайдер)"
    exit 1
fi
if [ ! -f .env ]; then
    err "не найден .env — скопируйте .env.example в .env и заполните обязательные поля"
    exit 1
fi

# Порт → compose-сервис и имя контейнера.
PORTS=(8080 8081 8082 8084 8085 8086 8087 8088 8089 8090 8091 8092)
declare -A PORT_SERVICE=(
    [8080]=gateway
    [8081]=torrent-service
    [8082]=stats-service
    [8084]=scheduler-service
    [8085]=history-service
    [8086]=auth-service
    [8087]=books-service
    [8088]=search-service
    [8089]=youtube-service
    [8090]=web-dashboard
    [8091]=media-service
    [8092]=music-service
)
declare -A SERVICE_CONTAINER=(
    [gateway]=nas-gateway
    [torrent-service]=nas-torrent
    [stats-service]=nas-stats
    [scheduler-service]=nas-scheduler
    [history-service]=nas-history
    [auth-service]=nas-auth
    [books-service]=nas-books
    [search-service]=nas-search
    [youtube-service]=nas-youtube
    [media-service]=nas-media
    [music-service]=nas-music
    [web-dashboard]=nas-dashboard
)
FAILED_PORTS=()

# ------------------------------------------------------------
# Вспомогательные функции
# ------------------------------------------------------------
env_get() {
    local key=$1 value
    [ -f .env ] || return 0
    value=$(sed -n "s/^[[:space:]]*\(export[[:space:]]\+\)\?${key}[[:space:]]*=[[:space:]]*//p" .env | tail -n 1)
    value=${value%$'\r'}
    value=${value#\"}; value=${value%\"}
    value=${value#\'}; value=${value%\'}
    printf '%s' "$value"
}

running() {
    "$PODMAN" ps --format '{{.Names}}' 2>/dev/null | grep -qx "$1"
}

# Порты, которые намеренно НЕ публикуются на хост (только внутри сети
# стека): auth-service, media-service, music-service. Проверять их через
# 127.0.0.1 на хосте бессмысленно — там их нет, и скрипт вечно считал их
# упавшими, пересоздавая контейнеры и роняя юнит с exit 1.
INTERNAL_PORTS=(8086 8091 8092)

is_internal_port() {
    local port=$1 p
    for p in "${INTERNAL_PORTS[@]}"; do
        [ "$p" = "$port" ] && return 0
    done
    return 1
}

http_code() {
    local port=$1 name
    if is_internal_port "$port"; then
        name=${SERVICE_CONTAINER[${PORT_SERVICE[$port]}]}
        if [ -n "$name" ] && "$PODMAN" exec "$name" \
            wget -q -O- -T 2 "http://127.0.0.1:${port}/health" >/dev/null 2>&1; then
            echo 200
        else
            echo 000
        fi
        return
    fi
    curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$1/health" 2>/dev/null || true
}

wait_healthy() {
    local name=$1 timeout=$2 waited=0 status health
    while [ "$waited" -lt "$timeout" ]; do
        status=$("$PODMAN" inspect -f '{{.State.Status}}' "$name" 2>/dev/null || true)
        health=$("$PODMAN" inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$name" 2>/dev/null || true)
        if [ "$status" = "running" ] && { [ "$health" = "healthy" ] || [ "$health" = "none" ]; }; then
            return 0
        fi
        sleep 2
        waited=$((waited + 2))
    done
    return 1
}

ensure_healthy() {
    local name=$1 timeout=$2
    if wait_healthy "$name" "$timeout"; then
        ok "$name готов"
        return 0
    fi
    warn "$name не стал healthy за ${timeout}s — перезапускаю"
    "$PODMAN" restart "$name" >/dev/null 2>&1 || true
    if wait_healthy "$name" 45; then
        ok "$name готов после перезапуска"
        return 0
    fi
    return 1
}

start_service() {
    local svc=$1 attempt out
    for attempt in 1 2 3; do
        if out=$("$PODMAN" compose up -d "$svc" 2>&1); then
            return 0
        fi
        warn "не удалось поднять $svc (попытка $attempt/3)"
        printf '%s\n' "$out" | tail -n 5 >&2
        sleep 3
    done
    return 1
}

wait_port() {
    local port=$1 timeout=$2 waited=0
    while [ "$waited" -lt "$timeout" ]; do
        [ "$(http_code "$port")" = "200" ] && return 0
        sleep 2
        waited=$((waited + 2))
    done
    return 1
}

# Ждём NAS: доступ к каталогу триггерит automount, затем проверяем,
# что CIFS действительно смонтирован. Без этого media-service падает
# на старте с "failed to fulfil mount request".
wait_for_media_dirs() {
    local waited=0
    local dirs=("$MEDIA_MOVIES_DIR" "$MEDIA_TV_DIR" "$MEDIA_PHOTOS_DIR" "$MUSIC_DIR")

    if [ -d /mnt/nas/root ] && command -v mountpoint >/dev/null 2>&1; then
        while [ "$waited" -lt 60 ]; do
            mountpoint -q /mnt/nas/root && break
            ls /mnt/nas/root >/dev/null 2>&1 || true
            sleep 2
            waited=$((waited + 2))
        done
        if ! mountpoint -q /mnt/nas/root; then
            warn "NAS /mnt/nas/root не смонтирован — media-service может не стартовать"
        fi
    fi

    local dir
    for dir in "${dirs[@]}"; do
        waited=0
        while [ ! -d "$dir" ] && [ "$waited" -lt 10 ]; do
            sleep 2
            waited=$((waited + 2))
        done
        if [ ! -d "$dir" ]; then
            warn "нет каталога $dir"
            return 1
        fi
    done
    return 0
}

# Проверяем все порты параллельно (общий дедлайн), а не по очереди.
check_ports() {
    local timeout=${1:-90} quiet=${2:-0}
    local deadline=$((SECONDS + timeout))
    local -A up=()
    local port code pending

    while [ "$SECONDS" -lt "$deadline" ]; do
        pending=0
        for port in "${PORTS[@]}"; do
            [ "${up[$port]:-0}" = "1" ] && continue
            code=$(http_code "$port")
            if [ "$code" = "200" ]; then
                up[$port]=1
                [ "$quiet" = "0" ] && ok "${PORT_SERVICE[$port]} (:${port})"
            else
                pending=$((pending + 1))
            fi
        done
        if [ "$pending" -eq 0 ]; then
            FAILED_PORTS=()
            return 0
        fi
        sleep 2
    done

    FAILED_PORTS=()
    for port in "${PORTS[@]}"; do
        [ "${up[$port]:-0}" = "1" ] || FAILED_PORTS+=("$port")
    done
    return 1
}

# force=0: restart/start; force=1: rm -f + recreate (лечит потерю port-forward).
recover_ports() {
    local force=${1:-0} port svc name out recreated=0

    for port in "${FAILED_PORTS[@]}"; do
        svc=${PORT_SERVICE[$port]}
        name=${SERVICE_CONTAINER[$svc]}

        if [ "$force" = "1" ]; then
            warn "$svc (:${port}) — пересоздаю контейнер $name"
            "$PODMAN" rm -f "$name" >/dev/null 2>&1 || true
            if ! out=$("$PODMAN" compose up -d --no-build "$svc" 2>&1); then
                warn "recreate $svc: $(printf '%s\n' "$out" | tail -n 3 | tr '\n' ' ')"
                "$PODMAN" compose up -d "$svc" >/dev/null 2>&1 || true
            fi
            recreated=1
        elif running "$name"; then
            warn "$svc (:${port}) не отвечает — restart $name"
            "$PODMAN" restart "$name" >/dev/null 2>&1 || true
        else
            warn "$svc (:${port}) не запущен — start $name"
            if ! "$PODMAN" start "$name" >/dev/null 2>&1; then
                warn "$svc не стартует — пересоздаю"
                "$PODMAN" rm -f "$name" >/dev/null 2>&1 || true
                if ! out=$("$PODMAN" compose up -d --no-build "$svc" 2>&1); then
                    printf '%s\n' "$out" | tail -n 5 >&2
                    continue
                fi
                recreated=1
            fi
        fi
    done

    # После пересозданий обновляем DNS/port-forward.
    "$PODMAN" network reload --all >/dev/null 2>&1 || true

    # Если IP какого-то сервиса изменился, gateway мог закэшировать старый.
    if [ "$recreated" = "1" ]; then
        warn "были пересоздания — обновляю gateway и dashboard"
        "$PODMAN" restart nas-gateway >/dev/null 2>&1 || true
        sleep 3
        "$PODMAN" restart nas-dashboard >/dev/null 2>&1 || true
        sleep 2
    fi
}

success() {
    echo ""
    ok "Все сервисы запущены и отвечают!"
    exit 0
}

print_failure_hints() {
    echo ""
    err "${#FAILED_PORTS[@]} сервисов не отвечают:"
    local port svc name
    for port in "${FAILED_PORTS[@]}"; do
        svc=${PORT_SERVICE[$port]}
        name=${SERVICE_CONTAINER[$svc]}
        echo "  - $svc (:${port}), контейнер $name"
        echo "    podman logs --tail 100 $name"
    done
    echo ""
    echo "  podman ps -a --format '{{.Names}}\t{{.Status}}'"
}

# ------------------------------------------------------------
# Основной сценарий
# ------------------------------------------------------------
echo "🚀 Запуск NAS-Stack..."

log "Сброс сети (DNS/port-forward)..."
"$PODMAN" network reload --all >/dev/null 2>&1 || true

log "1/7 Зависимости: postgres, redis"
"$PODMAN" compose up -d postgres redis
if ! ensure_healthy nas-postgres 90; then
    err "postgres не готов"
    "$PODMAN" logs --tail 50 nas-postgres 2>&1 || true
    exit 1
fi
if ! ensure_healthy nas-redis 45; then
    err "redis не готов"
    "$PODMAN" logs --tail 50 nas-redis 2>&1 || true
    exit 1
fi

log "2/7 NAS и media-service"
MEDIA_MOVIES_DIR="$(env_get MEDIA_MOVIES_HOST_DIR)"; MEDIA_MOVIES_DIR=${MEDIA_MOVIES_DIR:-/mnt/nas/root/FuzzyHD/Movies}
MEDIA_TV_DIR="$(env_get MEDIA_TV_HOST_DIR)";         MEDIA_TV_DIR=${MEDIA_TV_DIR:-/mnt/nas/root/FuzzyHD/TV}
MEDIA_PHOTOS_DIR="$(env_get MEDIA_PHOTOS_HOST_DIR)"; MEDIA_PHOTOS_DIR=${MEDIA_PHOTOS_DIR:-/mnt/nas/root/FuzzyHD/Photos}
MUSIC_DIR="$(env_get MUSIC_HOST_DIR)";               MUSIC_DIR=${MUSIC_DIR:-/mnt/nas/root/FuzzyHD/Music}
wait_for_media_dirs || warn "проверьте NAS-монтирование"
if start_service media-service; then
    ok "media-service запущен"
else
    warn "media-service не поднялся — повторю в health-check"
fi

log "3/7 Приложения"
CORE_SERVICES=(
    torrent-service stats-service scheduler-service history-service auth-service
    books-service search-service youtube-service music-service
)
for svc in "${CORE_SERVICES[@]}"; do
    log "  $svc"
    if start_service "$svc"; then
        :
    else
        warn "$svc не поднялся — проверю в health-check"
    fi
    sleep 1
done

log "4/7 gateway"
if start_service gateway; then
    ok "gateway запущен"
else
    warn "gateway не поднялся — проверю в health-check"
fi
wait_port 8080 45 || warn "gateway пока не отвечает"

log "5/7 web-dashboard"
if start_service web-dashboard; then
    ok "web-dashboard запущен"
else
    warn "web-dashboard не поднялся — проверю в health-check"
fi

log "6/7 Дополнительные сервисы"
for svc in ntfy flaresolverr prowlarr prometheus grafana; do
    if start_service "$svc"; then
        :
    else
        warn "$svc (опционально) не поднялся"
    fi
done

log "7/7 Проверка сервисов"
if check_ports 90; then
    success
fi

warn "Не все сервисы ответили — пробую restart/start"
recover_ports 0
sleep 3
if check_ports 45 1; then
    success
fi

warn "Повторная попытка с пересозданием контейнеров"
recover_ports 1
sleep 3
if check_ports 60 1; then
    success
fi

print_failure_hints
exit 1
