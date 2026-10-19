#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# add-indexers.sh — добавление трекеров в Prowlarr через REST API
# ============================================================
#
# Использование:
#   ./add-indexers.sh list
#   ./add-indexers.sh add <name> [--username X --password Y --cookie Z --tags tag1,tag2]
#   ./add-indexers.sh test <id>
#   ./add-indexers.sh delete <id>
#
# Публичные трекеры (только name):
#   1337x, The Pirate Bay, RARBG (если ещё жив в схеме), YTS
#
# Приватные трекеры (нужны креды):
#   rutracker  — username/password или cookie
#   nnmclub    — username/password
#   kinozal    — username/password
#   lostfilm   — username/password
#
# Параметры PROWLARR_URL и PROWLARR_API_KEY читаются из .env.
# API-ключ НЕ хардкодится в скрипте.
# ============================================================

# --- цвета ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m' # No Color

ok()   { echo -e "${GREEN}✅ $*${NC}"; }
fail() { echo -e "${RED}❌ $*${NC}"; }
warn() { echo -e "${YELLOW}⚠️  $*${NC}"; }

# --- читаем конфиг из .env ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/../.env"

if [[ ! -f "$ENV_FILE" ]]; then
    fail "Файл .env не найден: $ENV_FILE"
    exit 1
fi

# Загружаем переменные из .env (формат KEY=VALUE)
set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

PROWLARR_URL="${PROWLARR_URL:-}"
PROWLARR_API_KEY="${PROWLARR_API_KEY:-}"

# Жёсткая валидация: без URL и ключа работать нельзя
if [[ -z "$PROWLARR_URL" ]]; then
    fail "PROWLARR_URL не задан в .env"
    exit 1
fi

if [[ -z "$PROWLARR_API_KEY" ]]; then
    fail "PROWLARR_API_KEY не задан в .env"
    exit 1
fi

# убираем trailing slash
PROWLARR_URL="${PROWLARR_URL%/}"
API_BASE="$PROWLARR_URL/api/v1"

# --- проверка зависимостей ---
for cmd in curl jq; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        fail "Не найден $cmd. Установите: sudo apt install $cmd"
        exit 1
    fi
done

# --- обёртки над curl ---
# GET с флагом -f: при HTTP-ошибке сразу падаем
api_get() {
    curl -sS -f -H "X-Api-Key: $PROWLARR_API_KEY" "$API_BASE$1"
}

# POST с --fail-with-body: при 4xx/5xx выводим тело ответа
api_post() {
    local path="$1" body="$2" tmp bodyfile
    tmp="$(mktemp)"
    bodyfile="$(mktemp)"
    printf '%s' "$body" >"$bodyfile"
    if curl -sS --fail-with-body -X POST \
        -H "X-Api-Key: $PROWLARR_API_KEY" \
        -H "Content-Type: application/json" \
        --data-binary "@$bodyfile" "$API_BASE$path" >"$tmp" 2>&1; then
        cat "$tmp"
        rm -f "$tmp" "$bodyfile"
        return 0
    else
        local code=$?
        cat "$tmp" >&2
        rm -f "$tmp" "$bodyfile"
        return "$code"
    fi
}

api_delete() {
    curl -sS -f -X DELETE -H "X-Api-Key: $PROWLARR_API_KEY" "$API_BASE$1"
}

# ============================================================
# get_tag_id — найти id тега по имени (пусто, если тега нет)
# ============================================================
get_tag_id() {
    local name="$1"
    curl -sS "$PROWLARR_URL/api/v1/tag" \
        -H "X-Api-Key: $PROWLARR_API_KEY" \
        | jq -r --arg name "$name" '.[] | select(.label == $name) | .id' | head -1
}

# ============================================================
# get_default_app_profile_id — первый доступный App Profile (нужен appProfileId > 0)
# ============================================================
get_default_app_profile_id() {
    local id
    id="$(curl -sS -f "$PROWLARR_URL/api/v1/appprofile" \
        -H "X-Api-Key: $PROWLARR_API_KEY" \
        | jq -r '.[0].id // empty')"
    if [[ -z "$id" ]]; then
        fail "Не удалось получить App Profile из Prowlarr"
        exit 1
    fi
    echo "$id"
}

# ============================================================
# list_indexers — вывести список установленных индексаторов
# ============================================================
list_indexers() {
    local resp
    if ! resp="$(api_get "/indexer" 2>&1)"; then
        fail "Не удалось получить список индексаторов: $resp"
        exit 1
    fi
    echo "📡 Установленные индексаторы (Prowlarr):"
    echo "-----------------------------------------"
    if [[ "$(echo "$resp" | jq 'length')" == "0" ]]; then
        echo "  (пусто — индексаторы не добавлены)"
    else
        echo "$resp" | jq -r '.[] | "  #\(.id)  \(.name)  (enable=\(.enable))"'
    fi
}

# ============================================================
# add_indexer — найти схему по name, собрать JSON, POST /indexer
# ============================================================
add_indexer() {
    local name="${1:-}"
    shift || true

    local username="" password="" cookie=""
    local tags_names=""

    # разбор флагов
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --username) username="${2:-}"; shift 2 ;;
            --password) password="${2:-}"; shift 2 ;;
            --cookie)   cookie="${2:-}";   shift 2 ;;
            --tags)     tags_names="${tags_names:+$tags_names,}${2:-}"; shift 2 ;;
            *)
                warn "Неизвестный флаг: $1"
                shift
                ;;
        esac
    done

    # Разрешаем имена тегов в id (через запятую)
    local tag_ids=""
    if [[ -n "$tags_names" ]]; then
        local t
        IFS=',' read -ra tag_arr <<< "$tags_names"
        for t in "${tag_arr[@]}"; do
            t="${t// /}"
            [[ -z "$t" ]] && continue
            local tid
            tid="$(get_tag_id "$t")"
            if [[ -z "$tid" ]]; then
                fail "Тег '$t' не найден в Prowlarr"
                exit 1
            fi
            tag_ids="${tag_ids:+$tag_ids,}$tid"
        done
    fi

    local schema
    if ! schema="$(api_get "/indexer/schema" 2>&1)"; then
        fail "Не удалось получить схему индексаторов: $schema"
        exit 1
    fi

    # ищем схему по name (сначала точное совпадение, затем регистронезависимое)
    local def
    def="$(echo "$schema" | jq -c --arg name "$name" '.[] | select(.name == $name)')"
    if [[ -z "$def" ]]; then
        def="$(echo "$schema" | jq -c --arg name "$name" '.[] | select((.name | ascii_downcase) == ($name | ascii_downcase))')"
    fi

    if [[ -z "$def" || "$def" == "null" ]]; then
        fail "Индексатор '$name' не найден в схеме. Доступные:"
        echo "$schema" | jq -r '.[].name' | sort | sed 's/^/  - /'
        exit 1
    fi

    # получаем App Profile (Prowlarr требует appProfileId > 0)
    local app_profile_id
    app_profile_id="$(get_default_app_profile_id)"

    # собираем тело: enable=true, appProfileId, теги и креды в fields
    local body
    body="$(echo "$def" | jq -c \
        --arg name "$name" \
        --argjson appProfileId "$app_profile_id" \
        --arg u "$username" \
        --arg p "$password" \
        --arg c "$cookie" \
        --argjson tags "[$tag_ids]" '
        .name = $name
        | .enable = true
        | .appProfileId = $appProfileId
        | .tags = $tags
        | .fields = (.fields // [] | map(
            if ($u != "" and ((.name | ascii_downcase) == "username")) then .value = $u
            elif ($p != "" and ((.name | ascii_downcase) == "password")) then .value = $p
            elif ($c != "" and ((.name | ascii_downcase) | contains("cookie"))) then .value = $c
            else . end
          ))
    ')"

    local resp
    if resp="$(api_post "/indexer" "$body")"; then
        local new_id
        new_id="$(echo "$resp" | jq -r '.id // empty')"
        ok "Индексатор '$name' добавлен (id=$new_id)"
    else
        fail "Ошибка добавления '$name' (см. выше)"
        exit 1
    fi
}

# ============================================================
# test_indexer — проверить индексатор по id
# ============================================================
test_indexer() {
    local id="${1:-}"

    local conf
    if ! conf="$(api_get "/indexer/$id" 2>&1)"; then
        fail "Индексатор id=$id не найден: $conf"
        exit 1
    fi

    local name
    name="$(echo "$conf" | jq -r '.name // "?"')"

    local resp
    if resp="$(api_post "/indexer/test" "$conf" 2>&1)"; then
        ok "Тест индексатора #$id ($name) пройден: $resp"
    else
        fail "Тест индексатора #$id ($name) провален: $resp"
        exit 1
    fi
}

# ============================================================
# delete_indexer — удалить индексатор с подтверждением
# ============================================================
delete_indexer() {
    local id="${1:-}"

    local name
    name="$(api_get "/indexer/$id" 2>/dev/null | jq -r '.name // "?"')" || name="?"

    echo "Вы действительно хотите удалить индексатор #$id ($name)?"
    read -r -p "Введите 'да' для подтверждения: " confirm
    if [[ "$confirm" != "да" && "$confirm" != "Да" && "$confirm" != "yes" && "$confirm" != "y" ]]; then
        warn "Отменено, индексатор не удалён."
        exit 0
    fi

    if api_delete "/indexer/$id" >/dev/null; then
        ok "Индексатор #$id ($name) удалён"
    else
        fail "Не удалось удалить индексатор #$id"
        exit 1
    fi
}

# ============================================================
# usage
# ============================================================
usage() {
    echo "Использование:"
    echo "  $0 list"
    echo "  $0 add <name> [--username X --password Y --cookie Z --tags tag1,tag2]"
    echo "  $0 test <id>"
    echo "  $0 delete <id>"
}

# ============================================================
# main
# ============================================================
case "${1:-}" in
    list)
        list_indexers
        ;;
    add)
        [[ -z "${2:-}" ]] && { usage; exit 1; }
        add_indexer "${@:2}"
        ;;
    test)
        [[ -z "${2:-}" ]] && { usage; exit 1; }
        test_indexer "$2"
        ;;
    delete)
        [[ -z "${2:-}" ]] && { usage; exit 1; }
        delete_indexer "$2"
        ;;
    *)
        usage
        exit 1
        ;;
esac
