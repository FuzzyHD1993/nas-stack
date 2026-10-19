#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# update-rutracker-cookie.sh
# Обновляет RUTRACKER_COOKIE в .env и перезапускает сервисы.
#
# Использование:
#   ./scripts/update-rutracker-cookie.sh                  # спросит bb_session и bb_data
#   ./scripts/update-rutracker-cookie.sh <bb_session> <bb_data>
#
# Как получить cookie:
#   1. Открой https://rutracker.org в браузере (залогинен)
#   2. F12 → Application → Cookies → https://rutracker.org
#      (или Network → запрос к rutracker.org → Request Headers → Cookie)
#   3. Найди значения bb_session и bb_data
# ============================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/../.env"

if [[ ! -f "$ENV_FILE" ]]; then
    echo "❌ Файл .env не найден: $ENV_FILE" >&2
    exit 1
fi

BB_SESSION="${1:-}"
BB_DATA="${2:-}"

if [[ -z "$BB_SESSION" ]]; then
    read -r -p "bb_session: " BB_SESSION
fi
if [[ -z "$BB_DATA" ]]; then
    read -r -p "bb_data (можно пропустить): " BB_DATA
fi

if [[ -z "$BB_SESSION" ]]; then
    echo "❌ bb_session пуст — отмена" >&2
    exit 1
fi

COOKIE="bb_session=$BB_SESSION"
if [[ -n "$BB_DATA" ]]; then
    COOKIE="$COOKIE; bb_data=$BB_DATA"
fi

# Обновляем/добавляем строку RUTRACKER_COOKIE (awk — безопасно для спецсимволов)
TMP="$(mktemp)"
awk -v cookie="$COOKIE" '
    /^RUTRACKER_COOKIE=/ { print "RUTRACKER_COOKIE=" cookie; done=1; next }
    { print }
    END { if (!done) print "RUTRACKER_COOKIE=" cookie }
' "$ENV_FILE" >"$TMP"
mv "$TMP" "$ENV_FILE"
chmod 600 "$ENV_FILE" 2>/dev/null || true
echo "✅ RUTRACKER_COOKIE обновлён в $ENV_FILE"

# Перезапуск сервисов
if command -v podman >/dev/null 2>&1; then
    podman restart nas-torrent >/dev/null 2>&1 \
        && echo "♻️  nas-torrent перезапущен" \
        || echo "⚠️  не удалось перезапустить nas-torrent"
    podman restart nas-prowlarr >/dev/null 2>&1 \
        && echo "♻️  nas-prowlarr перезапущен" \
        || echo "⚠️  не удалось перезапустить nas-prowlarr"
else
    echo "⚠️  podman не найден — перезапусти сервисы вручную"
fi

# Проверка профиля RuTracker (torrent-service :8081)
sleep 4
echo "🔎 Проверка профиля RuTracker (torrent-service):"
if curl -sS --max-time 60 "http://localhost:8081/rutracker/profile?refresh=1" 2>/dev/null; then
    echo
    echo "✅ Готово"
else
    echo "⚠️  torrent-service недоступен на :8081 (проверь вручную)"
fi
