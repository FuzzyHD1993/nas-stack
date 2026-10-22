#!/usr/bin/env bash
set -euo pipefail

# ============================================================
# update-indexer-definitions.sh — синхронизация определений
# индексаторов Prowlarr с зеркалом GitHub.
#
# Зачем: собственный обновлятор Prowlarr качает
# https://indexers.prowlarr.com/master/<SCHEMA>/package.zip,
# но с этой сети тот отдаёт HTTP 403 (Cloudflare «Attention
# Required»). Обход: тот же набор определений лежит в
# github.com/Prowlarr/Indexers → definitions/v<SCHEMA>/*.yml,
# качаем tarball с codeload.github.com и кладём файлы в
# /config/Definitions контейнера nas-prowlarr через podman cp.
#
# ВАЖНО:
#  - /config/Definitions/Custom/ — пользовательские определения;
#    скрипт НИКОГДА не трогает и не очищает этот каталог;
#  - /config контейнера — rootless-том podman: по host-пути
#    монтирования (выясняется через podman inspect) хостовый
#    юзер МОЖЕТ читать файлы, но НЕ может писать в них
#    (uid-маппинг: container-uid 1000 → host-uid 100999).
#    Поэтому все ЧТЕНИЯ (debug-лог, маркер .sync-source,
#    подсчёт .yml) идут напрямую с хоста по пути монтирования,
#    а все ЗАПИСИ — только через podman cp;
#  - «podman-exec» (запуск процессов внутри контейнера) НЕ
#    используется вообще — и это намеренно: из systemd user-юнита
#    без полной cgroup-делегации он падает с ошибкой
#    «nsexec: failed to update /proc/self/oom_score_adj:
#    Permission denied: OCI permission denied» (runc setns).
#    podman cp и podman inspect из юнита при этом работают;
#  - Prowlarr читает определения с диска только при старте
#    контейнера, поэтому после обновления файлов, если сам
#    Prowlarr их не перечитал, скрипт делает podman restart.
#
# Использование:
#   ./update-indexer-definitions.sh           # обычная синхронизация
#   ./update-indexer-definitions.sh --force   # переустановить, даже если sha совпал
#   ./update-indexer-definitions.sh --status  # только показать состояние (без изменений)
#
# PROWLARR_URL и PROWLARR_API_KEY читаются из .env, ключ
# нигде не печатается и в контейнер не передаётся.
# ============================================================

CONTAINER="nas-prowlarr"
# --- пути ВНУТРИ контейнера (используются ТОЛЬКО как container-путь
# в podman cp и как имена; с хоста напрямую их НЕ читают и туда НЕ пишут) ---
DEFDIR="/config/Definitions"          # путь ВНУТРИ контейнера
LOG_FILE="/config/logs/prowlarr.debug.txt"   # debug-лог Prowlarr (НЕ внутри Definitions/)
MARKER_FILE="$DEFDIR/.sync-source"
# --- host-пути тех же каталогов/файлов резолвятся через CONFIG_HOST (см. ниже) ---

TAR_URL="https://codeload.github.com/Prowlarr/Indexers/tar.gz/refs/heads/master"
VERSIONS_URL="https://raw.githubusercontent.com/Prowlarr/Indexers/master/VERSIONS"
COMMITS_API="https://api.github.com/repos/Prowlarr/Indexers/commits"

LOCK_FILE="${XDG_RUNTIME_DIR:-/tmp}/nas-prowlarr-definitions.lock"
TMP_DIR=""

FORCE=0
STATUS_ONLY=0

# --- цвета и вывод (как в add-indexers.sh) ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

ok()   { echo -e "${GREEN}✅ $*${NC}"; }
fail() { echo -e "${RED}❌ $*${NC}" >&2; }
warn() { echo -e "${YELLOW}⚠️  $*${NC}"; }

usage() {
    echo "Использование:"
    echo "  $0            — синхронизация определений индексаторов"
    echo "  $0 --force    — переустановить даже если upstream не менялся"
    echo "  $0 --status   — показать текущее состояние (только чтение)"
}

# --- разбор флагов ---
# --status определён как строго read-only: если нужно только
# состояние — флаг --force игнорируется.
for arg in "$@"; do
    case "$arg" in
        --force)  FORCE=1 ;;
        --status) STATUS_ONLY=1 ;;
        *)
            fail "Неизвестный флаг: $arg"
            usage
            exit 1
            ;;
    esac
done

# --- читаем конфиг из .env ---
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ENV_FILE="$REPO_DIR/.env"

if [[ ! -f "$ENV_FILE" ]]; then
    fail "Файл .env не найден: $ENV_FILE"
    exit 1
fi

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

PROWLARR_URL="${PROWLARR_URL:-http://127.0.0.1:9696}"
PROWLARR_URL="${PROWLARR_URL%/}"
API_BASE="$PROWLARR_URL/api/v1"
PROWLARR_API_KEY="${PROWLARR_API_KEY:-}"

if [[ -z "$PROWLARR_API_KEY" && "$STATUS_ONLY" -eq 0 ]]; then
    fail "PROWLARR_API_KEY не задан в .env"
    exit 1
fi

# --- зависимости на хосте ---
for cmd in curl jq tar podman; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        fail "Не найден $cmd — он обязателен для работы скрипта"
        exit 1
    fi
done

# ============================================================
# Host-путь монтирования /config контейнера (rootless-том podman).
# Резолвим через podman inspect РОВНО ОДИН раз в начале: запуск
# процессов внутри контейнера из systemd user-юнита падает на
# runc setns (oom_score_adj: Permission denied), а podman
# inspect и podman cp работают. Читаем этот путь напрямую с
# хоста (файлы доступны на чтение), пишем только через podman cp.
# ============================================================
CONFIG_HOST="$( (podman inspect "$CONTAINER" --format \
        '{{range .Mounts}}{{if eq .Destination "/config"}}{{.Source}}{{end}}{{end}}' \
        2>/dev/null || true) | tr -d '[:space:]' )"
if [[ -z "$CONFIG_HOST" ]]; then
    fail "Не удалось найти host-путь монтирования /config контейнера $CONTAINER (podman inspect вернул пустое значение). Проверьте, что контейнер и его том nas-stack_prowlarr-config на месте."
    exit 1
fi

# host-пути тех же каталогов/файлов, что и DEFDIR/LOG_FILE/MARKER_FILE
DEFDIR_HOST="$CONFIG_HOST/Definitions"
LOG_FILE_HOST="$CONFIG_HOST/logs/prowlarr.debug.txt"
MARKER_FILE_HOST="$DEFDIR_HOST/.sync-source"

# ============================================================
# Вспомогательные функции
# ============================================================

# num <string> — нормализовать строку в целое число (иначе 0)
num() {
    case "${1:-}" in
        ''|*[!0-9]*) echo 0 ;;
        *)           echo "$1" ;;
    esac
}

# Сколько записей "Loading definition" сейчас в debug-логе
# контейнера. Читаем файл напрямую с хоста по пути монтирования
# тома (выполнение процессов внутри контейнера не используется).
# grep может не сработать (нет файла или 0 совпадений) — это
# норм, считаем как 0, поэтому || true перед tr.
count_loading() {
    local n
    n="$( (grep -c 'Loading definition' "$LOG_FILE_HOST" 2>/dev/null || true) | tr -d '[:space:]' )"
    num "$n"
}

# Сколько файлов .yml в каталоге определений контейнера
# (считаем по host-пути монтирования)
count_definitions() {
    local n
    n="$( (find "$DEFDIR_HOST" -maxdepth 1 -type f -name '*.yml' 2>/dev/null || true) | wc -l | tr -d '[:space:]' )"
    num "$n"
}

# Прочитать поле маркера MARKER_FILE (читаем с хоста по пути
# монтирования тома; файл доступен на чтение; read-only)
read_marker_field() {
    local field="$1"
    (sed -n "s/^${field}=//p" "$MARKER_FILE_HOST" 2>/dev/null || true) | head -n 1
}

# GET {API}/system/status → версия Prowlarr, или небоевой выход
api_version() {
    local out v
    if out="$(curl -sS --connect-timeout 10 --max-time 15 \
            -H "X-Api-Key: $PROWLARR_API_KEY" \
            "$API_BASE/system/status" 2>/dev/null)"; then
        v="$(printf '%s\n' "$out" | jq -r '.version // empty' 2>/dev/null || true)"
        if [[ -n "$v" ]]; then
            echo "$v"
            return 0
        fi
    fi
    return 1
}

# Ждать, пока API отвечает с версией, не дольше $1 сек (шаг 3 с)
wait_api_ready() {
    local deadline="$1" waited=0 ver
    while (( waited < deadline )); do
        if ver="$(api_version)"; then
            echo "$ver"
            return 0
        fi
        sleep 3
        waited=$((waited + 3))
    done
    return 1
}

# ============================================================
# detect_schema — какую версию схемы хочет этот Prowlarr.
# Приоритет:
#   а) его собственный debug-лог (строка про package.zip);
#   б) CURRENT_VERSION из GitHub (файл VERSIONS);
#   в) жёсткий дефолт 11.
# ============================================================
detect_schema() {
    local n cur
    # Лог читаем с хоста: запуск процессов внутри контейнера из
    # systemd user-юнита не работает (см. шапку).
    if [[ -f "$LOG_FILE_HOST" ]]; then
        n="$( (grep -ohE 'indexers[\.]prowlarr[\.]com/master/[0-9]+/package[\.]zip' "$LOG_FILE_HOST" 2>/dev/null | tail -n 1 || true) \
                | grep -oE '[0-9]+' | tail -n 1 || true )"
        if [[ -n "$n" && "$n" =~ ^[0-9]+$ ]]; then
            echo "$n"
            return 0
        fi
    fi
    if cur="$(curl -sS -f --connect-timeout 15 --max-time 30 "$VERSIONS_URL" 2>/dev/null)"; then
        cur="$(printf '%s\n' "$cur" | sed -n 's/^CURRENT_VERSION=//p' | head -n 1 | tr -d '[:space:]')"
        if [[ -n "$cur" && "$cur" =~ ^[0-9]+$ ]]; then
            echo "$cur"
            return 0
        fi
    fi
    echo "11"
}

# ============================================================
# --status: только чтение, без изменений и без рестартов.
# ============================================================
if (( STATUS_ONLY )); then
    schema="$(detect_schema)"
    defs_n="$(count_definitions)"
    recorded="$(read_marker_field UPSTREAM_SHA)"
    last_sync="$(read_marker_field SYNC_TIME)"
    source_url="$(read_marker_field SOURCE_URL)"
    marker_schema="$(read_marker_field SCHEMA)"

    echo "Состояние синхронизации определений Prowlarr:"
    echo "  Контейнер:                $CONTAINER"
    echo "  Схема, которую хочет Prowlarr: v$schema"
    echo "  Файлов .yml в $DEFDIR:  $defs_n"
    echo "  Маркер .sync-source:      $( [[ -f "$MARKER_FILE_HOST" ]] && echo 'есть' || echo 'нет')"
    if [[ -n "$marker_schema" || -n "$recorded" || -n "$last_sync" ]]; then
        echo "  Схема из маркера:         ${marker_schema:-(нет)}"
        echo "  Upstream sha:             ${recorded:-(нет)}"
        echo "  Последняя синхронизация:  ${last_sync:-(нет)}"
        echo "  Источник:                 ${source_url:-(нет)}"
    fi
    exit 0
fi

# ============================================================
# Блокировка от параллельных запусков (таймер + ручной запуск)
#
# ВАЖНО: лок-файл НЕльзя держать на «exec 9>>» — этот fd
# наследуют все потомки, включая conmon, который podman
# запускает для nas-prowlarr. conmon живёт дольше скрипта и
# держал бы лок вечно: все следующие запуски (включая
# ежедневный таймер) молча пропускались бы с сообщением
# «уже запущен другой экземпляр».
#
# Поэтому запускаем сами себя под flock с -o (--close):
# flock держит лок, а в дочернем процессе fd закрывается
# ДО exec — наследовать его уже никто не может.
# ============================================================
if [[ -z "${DEFINITIONS_SYNC_LOCKED:-}" ]]; then
    if command -v flock >/dev/null 2>&1; then
        export DEFINITIONS_SYNC_LOCKED=1
        flock -n -o -E 75 "$LOCK_FILE" "$0" "$@"
        rc=$?
        if [[ $rc -eq 75 ]]; then
            ok "Уже запущен другой экземпляр ($LOCK_FILE) — выхожу"
            exit 0
        fi
        exit $rc
    fi
    warn "flock не найден — работаю БЕЗ блокировки от параллельного запуска"
fi

trap 'rm -rf "$TMP_DIR"' EXIT

# ============================================================
# 1. Какую схему качаем
# ============================================================
schema="$(detect_schema)"
ok "Prowlarr хочет схему definitions/v$schema"

# ============================================================
# 2. Свежий commit-sha каталога definitions/v<SCHEMA> на GitHub
# ============================================================
sha=""
if resp="$(curl -sS -f --connect-timeout 15 --max-time 60 \
        "$COMMITS_API?path=definitions/v${schema}&per_page=1" 2>/dev/null)"; then
    sha="$(printf '%s\n' "$resp" | jq -r '.[0].sha // empty' 2>/dev/null || true)"
fi

recorded="$(read_marker_field UPSTREAM_SHA)"
marker_schema="$(read_marker_field SCHEMA)"

if [[ -z "$sha" ]]; then
    warn "Не удалось получить commit-sha из GitHub API — выполняю полную синхронизацию без сравнения sha"
elif [[ "$sha" == "$recorded" && "$schema" == "$marker_schema" && "$FORCE" -eq 0 ]]; then
    ok "Определения уже актуальны (sha ${sha:0:12}, схема v$schema) — копирование и рестарт не нужны"
    exit 0
fi

# ============================================================
# 3. Скачать и проверить tarball
# ============================================================
TMP_DIR="$(mktemp -d)"
ok "Качаю архив с GitHub: $TAR_URL"

if ! curl -sS -f --retry 3 --retry-delay 3 --connect-timeout 15 --max-time 300 \
        -o "$TMP_DIR/indexers.tar.gz" "$TAR_URL"; then
    fail "Не удалось скачать архив с $TAR_URL"
    exit 1
fi

if [[ ! -s "$TMP_DIR/indexers.tar.gz" ]]; then
    fail "Скачанный архив пуст"
    exit 1
fi

if ! tar -tzf "$TMP_DIR/indexers.tar.gz" >/dev/null 2>&1; then
    fail "Скачанный файл не является корректным tar.gz"
    exit 1
fi

# ============================================================
# 4. Распаковать ТОЛЬКО definitions/v<SCHEMA> — каталог
#    схемы внутри tarball'а это Indexers-master/definitions/v<SCHEMA>,
#    strip-components=3 кладёт содержимое прямо в defs/
# ============================================================
mkdir -p "$TMP_DIR/defs"
if ! tar -xzf "$TMP_DIR/indexers.tar.gz" --strip-components=3 \
        -C "$TMP_DIR/defs" "Indexers-master/definitions/v${schema}"; then
    fail "В архиве нет Indexers-master/definitions/v$schema — версия схемы неверна или изменилась структура репозитория"
    fail "Чтобы узнать нужную схему, посмотрите: $0 --status"
    exit 1
fi

yml_count="$(find "$TMP_DIR/defs" -maxdepth 1 -type f -name '*.yml' | wc -l | tr -d '[:space:]')"
if [[ "$(num "$yml_count")" -eq 0 ]]; then
    fail "После распаковки definitions/v$schema не нашлось ни одного .yml — версия схемы неверна или изменилась структура репозитория"
    exit 1
fi
if [[ ! -f "$TMP_DIR/defs/schema.json" ]]; then
    warn "В definitions/v$schema нет schema.json — возможно, изменилась структура репозитория"
fi

# ============================================================
# 5. Установить в контейнер.
#    Копируем ТОЛЬКО содержимое defs/ — файлы .yml и schema.json
#    перезаписываются, каталог Custom/ в архиве отсутствует,
#    поэтому остаётся нетронутым. Базу Prowlarr не трогаем,
#    ничего не удаляем.
# ============================================================
ok "Устанавливаю $yml_count определений в контейнер ($CONTAINER:$DEFDIR/)"
if ! podman cp "$TMP_DIR/defs/." "$CONTAINER:$DEFDIR/"; then
    fail "podman cp: не удалось скопировать определения в $CONTAINER:$DEFDIR/"
    exit 1
fi

# ============================================================
# 6. Обновить маркер .sync-source. Хост в rootless-том писать
#    НЕ может (uid-маппинг), поэтому: пишем маркер во временный
#    файл внутри $TMP_DIR (чистится trap'ом), затем ставим его
#    в контейнер через podman cp на CONTAINER-путь (host-путь
#    здесь не подходит — писать в том напрямую нельзя).
#    Креды в маркер не пишем.
# ============================================================
now="$(date -u '+%Y-%m-%d %H:%M:%S UTC')"
cat > "$TMP_DIR/.sync-source" <<MARKER
SOURCE_URL=$TAR_URL
SCHEMA=$schema
UPSTREAM_SHA=$sha
SYNC_TIME=$now
NOTE=Синхронизация через зеркало GitHub (Prowlarr/Indexers): indexers.prowlarr.com отдаёт HTTP 403 (Cloudflare) из этой сети.
MARKER
if ! podman cp "$TMP_DIR/.sync-source" "$CONTAINER:$DEFDIR/.sync-source"; then
    fail "podman cp: не удалось записать маркер в $CONTAINER:$DEFDIR/.sync-source"
    exit 1
fi

# ============================================================
# 7. Попросить Prowlarr перечитать определения.
#    Не-2xx — предупреждение, а не ошибка: встроенный обновлятор
#    всё равно может упереться в Cloudflare 403. Файлы уже на
#    диске, гарантию применения даёт следующий шаг.
# ============================================================
loading_before="$(count_loading)"

code="$( (curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 10 --max-time 30 \
        -X POST -H "X-Api-Key: $PROWLARR_API_KEY" \
        -H "Content-Type: application/json" \
        -d '{"name":"IndexerDefinitionUpdate"}' \
        "$API_BASE/command" 2>/dev/null || true) )"
if [[ "$code" =~ ^2 ]]; then
    ok "Prowlarr принял команду IndexerDefinitionUpdate (HTTP $code)"
else
    warn "Prowlarr ответил HTTP ${code:-нет ответа} на IndexerDefinitionUpdate — это ожидаемо (его встроенный обновлятор упирается в Cloudflare 403); файлы уже на диске, применяю их ниже"
fi

# ============================================================
# 8. Гарантировать применение: Prowlarr читает определения с
#    диска только при старте. Ждём до 60 с рост числа строк
#    "Loading definition"; если их нет — рестартуем контейнер.
# ============================================================
ver=""
reloaded=0
waited=0
while (( waited < 60 )); do
    loading_after="$(count_loading)"
    if (( loading_after > loading_before )); then
        reloaded=1
        break
    fi
    sleep 5
    waited=$((waited + 5))
done

if (( reloaded )); then
    ok "Prowlarr сам перечитал определения через API (строк 'Loading definition': $loading_before → $loading_after)"
    if ! ver="$(wait_api_ready 30)"; then
        fail "Prowlarr не отвечает через API после перезагрузки определений"
        exit 1
    fi
else
    warn "API-перезагрузка не подтвердилась за 60 с — перезапускаю контейнер (новые определения применятся при старте)"
    if ! podman restart "$CONTAINER" >/dev/null; then
        fail "podman restart $CONTAINER не удался"
        exit 1
    fi
    if ! ver="$(wait_api_ready 120)"; then
        fail "Контейнер $CONTAINER не поднялся и не ответил через API за 120 с после рестарта"
        exit 1
    fi
    ok "Контейнер $CONTAINER перезапущен; определения применены при старте (Prowlarr версии $ver)"
fi

# ============================================================
# 9. Итоговая проверка и сводка
# ============================================================
defs_n="$(count_definitions)"

echo ""
echo "================ Итоговая сводка ================"
echo "✅ Prowlarr отвечает: контейнер $CONTAINER, версия $ver"
echo "✅ Определений (.yml) в $DEFDIR: $defs_n"

if ! indexers_json="$(curl -sS -f --connect-timeout 10 --max-time 30 \
        -H "X-Api-Key: $PROWLARR_API_KEY" \
        "$API_BASE/indexer" 2>/dev/null)"; then
    fail "Не удалось получить список индексаторов из $API_BASE/indexer"
    exit 1
fi

n_indexers="$(printf '%s\n' "$indexers_json" | jq -r 'if type=="array" then length else 0 end' 2>/dev/null || true)"
n_indexers="$(num "$n_indexers")"
if (( n_indexers == 0 )); then
    warn "В Prowlarr не добавлено ни одного индексатора"
else
    echo "📡 Индексаторы:"
    printf '%s\n' "$indexers_json" \
        | jq -r '.[] | "  \(if .enable then "✅" else "❶" end) #\(.id)  \(.name)  enable=\(.enable)"' \
        2>/dev/null || printf '%s\n' "$indexers_json" | jq -r '.[] | "  #\(.id)  \(.name)  enable=\(.enable)"'
fi

disabled_names="$(printf '%s\n' "$indexers_json" | jq -r '[.[] | select(.enable != true) | .name] | join(", ")' 2>/dev/null || true)"
if [[ -n "$disabled_names" ]]; then
    fail "ВНИМАНИЕ: часть индексаторов ОТКЛЮЧЕНА (enable=false): $disabled_names"
fi

enabled_ids="$(printf '%s\n' "$indexers_json" | jq -r '[.[] | select(.enable == true) | .id] | join(" ")' 2>/dev/null || true)"

# Пробный поиск по каждому включённому индексатору: один
# недоступный трекер не должен валить весь скрипт.
zero_list=""
err_list=""
total=0
if [[ -n "$enabled_ids" ]]; then
    echo "🔎 Пробный поиск 'ubuntu' по включённым индексаторам:"
    for id in $enabled_ids; do
        name="$(printf '%s\n' "$indexers_json" | jq -r --argjson id "$id" '.[] | select(.id == $id) | .name' 2>/dev/null || echo "id=$id")"
        resp="$(curl -sS -G --connect-timeout 10 --max-time 90 \
                -H "X-Api-Key: $PROWLARR_API_KEY" \
                --data-urlencode "q=ubuntu" \
                --data-urlencode "type=search" \
                --data-urlencode "indexerIds=$id" \
                "$API_BASE/search" 2>/dev/null || true)"
        found="$(printf '%s\n' "$resp" | jq -r 'if type=="array" then length else -1 end' 2>/dev/null || echo "-1")"
        if [[ "$found" == "-1" ]]; then
            err_list="${err_list:+$err_list, }$name (#$id)"
            echo "  ⚠️  #$id $name: ошибка/таймаут поиска"
        else
            total=$((total + found))
            echo "  ✅ #$id $name: $found результатов"
            if (( found == 0 )); then
                zero_list="${zero_list:+$zero_list, }$name (#$id)"
            fi
        fi
    done
    echo "  Всего результатов: $total"
    if [[ -n "$zero_list" ]]; then
        warn "Вернули 0 результатов (трекер может быть недоступен или нужен логин): $zero_list"
    fi
    if [[ -n "$err_list" ]]; then
        warn "Поиск не удался (таймаут/ошибка): $err_list"
    fi
fi

ok "Синхронизация определений завершена (схема v$schema, sha ${sha:0:12})"
exit 0
