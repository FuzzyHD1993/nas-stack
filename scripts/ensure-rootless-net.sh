#!/bin/bash
# ensure-rootless-net.sh — проверяет и чинит rootless-сеть podman для стека NAS.
#
# Симптом: в сети nas-stack_default пропадает IPv4-маршрут по умолчанию.
# Тогда контейнеры остаются без DNS и без интернета и не видят хост
# (host.containers.internal -> notify-service на :8083, MPD на :6600).
#
# Причина: rootless netns создаётся при первом старте контейнера в bridge-сети.
# Если это происходит на старте ОС, когда сеть хоста ещё не готова
# (NetworkManager переименовывает eth0 -> eno1 и поднимает default route),
# pasta клонирует не тот интерфейс: в netns остаётся только IPv6, а маршрута
# по умолчанию для IPv4 нет. Сеть "залипает" до пересоздания netns.
#
# Использование:
#   scripts/ensure-rootless-net.sh            # проверка; exit 1, если сеть битая
#   scripts/ensure-rootless-net.sh --repair   # починить (пересоздать netns)
#
# После --repair контейнеры нужно поднять заново: podman compose up -d
set -uo pipefail

PODMAN="${PODMAN:-podman}"
NETNS_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/containers/networks/rootless-netns"

log() { echo "▶ $*"; }
ok()  { echo "✅ $*"; }
err() { echo "❌ $*" >&2; }

# Есть ли в rootless netns маршрут по умолчанию для IPv4.
netns_has_default_route() {
    "$PODMAN" unshare --rootless-netns ip -4 route show default 2>/dev/null | grep -q '^default'
}

netns_state() {
    "$PODMAN" unshare --rootless-netns ip -4 route show default 2>/dev/null | head -1
}

if netns_has_default_route; then
    ok "rootless-сеть podman исправна ($(netns_state))"
    exit 0
fi

if [ "${1:-}" != "--repair" ]; then
    err "в rootless netns нет IPv4-маршрута по умолчанию — контейнеры останутся без сети"
    err "починить: $0 --repair && podman compose up -d"
    exit 1
fi

log "пересоздаю rootless netns (сеть стека осталась без маршрута)"

# Контейнеры удерживают старый netns за собой — их нужно убрать, иначе
# пересоздания не произойдёт.
"$PODMAN" stop --all --time 15 >/dev/null 2>&1
ids=$("$PODMAN" ps -aq 2>/dev/null)
if [ -n "$ids" ]; then
    # shellcheck disable=SC2086
    "$PODMAN" rm -f $ids >/dev/null 2>&1
fi

# pasta держит netns — убиваем, чтобы podman создал новый.
pkill -f 'pasta --config-net' >/dev/null 2>&1
sleep 1
rm -rf "$NETNS_DIR"
"$PODMAN" network prune -f >/dev/null 2>&1

ok "rootless netns пересоздан"
log "поднимите контейнеры: podman compose up -d"
exit 0
