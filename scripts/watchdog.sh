#!/bin/bash
# watchdog.sh — следит за NAS-монтированием и перезапускает нездоровые контейнеры.
REPO_DIR="$(cd "$(dirname "$(readlink -f "$0")")/.." && pwd)"
cd "$REPO_DIR" || exit 1

NOTIFY_URL="${NOTIFY_URL:-http://localhost:8083}"

notify() {
    curl -sS -X POST "$NOTIFY_URL/send" \
        -H "Content-Type: application/json" \
        -d "{\"text\": \"$1\", \"severity\": \"$2\"}" >/dev/null 2>&1 || true
}

# ---- NAS mount watchdog ----
# Если /mnt/nas/root отвалился — пробуем перемонтировать (nofail/automount в fstab).
if ! mountpoint -q /mnt/nas/root 2>/dev/null; then
    echo "⚠️ /mnt/nas/root не смонтирован — пробую mount -a"
    notify "⚠️ NAS отвалился — перемонтирую" "warning"
    sudo -n mount -a >/dev/null 2>&1 || sudo -n systemctl start mnt-nas.mount mnt-nas-root.mount >/dev/null 2>&1
    sleep 5
    if mountpoint -q /mnt/nas/root 2>/dev/null; then
        echo "✅ NAS перемонтирован"
        notify "✅ NAS перемонтирован" "info"
    else
        echo "❌ NAS всё ещё недоступен"
        notify "❌ NAS недоступен — нужно вмешательство" "error"
    fi
fi

UNHEALTHY=$(podman ps --filter health=unhealthy --format "{{.Names}}" 2>/dev/null)

# Rootless-сеть podman. Если в rootless netns нет IPv4-маршрута, контейнеры
# живут без DNS и без интернета и не видят хост (host.containers.internal).
# Перезапуск контейнеров тут не помогает — нужен пересозданный netns.
if ! bash "$REPO_DIR/scripts/ensure-rootless-net.sh" >/dev/null 2>&1; then
    echo "⚠️ Rootless-сеть podman сломана — пересоздаю netns и стек"
    notify "⚠️ Rootless-сеть podman сломана — пересоздаю" "warning"
    bash "$REPO_DIR/scripts/ensure-rootless-net.sh" --repair >/dev/null 2>&1
    podman compose up -d >/dev/null 2>&1
    notify "✅ Rootless-сеть podman пересоздана, стек поднят" "info"
    exit 0
fi

if [ -n "$UNHEALTHY" ]; then
    echo "⚠️ Unhealthy: $UNHEALTHY"
    for c in $UNHEALTHY; do
        # Не трогаем контейнеры, которые ещё стартуют, и единичные сбои healthcheck.
        st=$(podman inspect "$c" --format '{{.State.Status}}' 2>/dev/null)
        fs=$(podman inspect "$c" --format '{{.State.Health.FailingStreak}}' 2>/dev/null)
        [ "$st" = "running" ] || continue
        [ "${fs:-0}" -ge 2 ] 2>/dev/null || continue
        echo "🔄 Restarting $c (failing streak=${fs})"
        podman restart "$c" >/dev/null 2>&1
        sleep 2
        if [ "$(podman inspect "$c" --format '{{.State.Status}}' 2>/dev/null)" != "running" ]; then
            podman start "$c" >/dev/null 2>&1
        fi
        notify "🔄 Auto-restart $c" "warning"
    done
fi

# Проверка RestartCount
podman ps -a --format "{{.Names}}" 2>/dev/null | while read -r c; do
    rc=$(podman inspect "$c" --format '{{.RestartCount}}' 2>/dev/null)
    if [ -n "$rc" ] && [ "$rc" -gt 5 ] 2>/dev/null; then
        echo "⚠️ $rc restarts: $c"
    fi
done
