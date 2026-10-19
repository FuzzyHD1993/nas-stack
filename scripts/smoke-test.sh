#!/bin/bash
# smoke-test.sh — smoke-тест всех сервисов и хост-демонов.

FAIL=0

if ! command -v jq >/dev/null 2>&1; then
    echo "❌ jq не установлен — нужен для E2E-проверок"
    exit 1
fi

test_endpoint() {
    local name=$1 url=$2 expected=$3
    local resp
    # 30s: первый запрос после истечения кэша music-index сканирует CIFS
    resp=$(curl -sS --max-time 30 "$url" 2>/dev/null)
    if echo "$resp" | grep -q "$expected"; then
        echo "✅ $name"
    else
        echo "❌ $name: $resp"
        FAIL=$((FAIL+1))
    fi
}

test_post() {
    local name=$1 url=$2 body=$3 expected=$4
    local resp
    resp=$(curl -sS --max-time 10 -X POST "$url" -H "Content-Type: application/json" -d "$body" 2>/dev/null)
    if echo "$resp" | grep -q "$expected"; then
        echo "✅ $name"
    else
        echo "❌ $name: $resp"
        FAIL=$((FAIL+1))
    fi
}

# Проверка HTTP-кода — для негативных security-тестов.
test_post_code() {
    local name=$1 url=$2 expected=$3
    local code
    code=$(curl -sS --max-time 10 -o /dev/null -w "%{http_code}" -X POST "$url" 2>/dev/null)
    if [ "$code" = "$expected" ]; then
        echo "✅ $name"
    else
        echo "❌ $name: HTTP $code (ожидался $expected)"
        FAIL=$((FAIL+1))
    fi
}

test_post_code_auth() {
    local name=$1 url=$2 token=$3 expected=$4
    local code
    code=$(curl -sS --max-time 10 -o /dev/null -w "%{http_code}" -X POST "$url" -H "X-Gateway-Token: $token" 2>/dev/null)
    if [ "$code" = "$expected" ]; then
        echo "✅ $name"
    else
        echo "❌ $name: HTTP $code (ожидался $expected)"
        FAIL=$((FAIL+1))
    fi
}

echo "=== Health контейнеров ==="
test_endpoint "gateway"    "http://localhost:8080/health" '"status":"ok"'
test_endpoint "torrent"    "http://localhost:8081/health" '"status":"ok"'
test_endpoint "stats"      "http://localhost:8082/health" '"status":"ok"'
test_endpoint "scheduler"  "http://localhost:8084/health" '"status":"ok"'
test_endpoint "history"    "http://localhost:8085/health" '"status":"ok"'
test_endpoint "auth"       "http://localhost:8086/health" '"status":"ok"'
test_endpoint "books"      "http://localhost:8087/health" '"status":"ok"'
test_endpoint "search"     "http://localhost:8088/health" '"status":"ok"'
test_endpoint "youtube"    "http://localhost:8089/health" '"status":"ok"'
test_endpoint "dashboard"  "http://localhost:8090/health" '"status":"ok"'
test_endpoint "media"      "http://localhost:8091/health" '"status":"ok"'
test_endpoint "music"      "http://localhost:8092/health" '"status":"ok"'

echo "=== Host-демоны ==="
test_endpoint "notify"     "http://localhost:8083/health" '"status":"ok"'
test_endpoint "host-agent" "http://localhost:9998/health" '"status":"ok"'
test_endpoint "ntfy"       "http://localhost:2586/v1/health" 'healthy'

echo "=== Инфра ==="
test_endpoint "grafana"    "http://localhost:3000/api/health" '"database"'
test_endpoint "prometheus" "http://localhost:9090/-/healthy" "Prometheus"
test_endpoint "gateway-status" "http://localhost:8080/status" '"auth"'

echo "=== v9.2 эндпоинты ==="
test_endpoint "smart-playlists" "http://localhost:8092/playlists/smart?type=recent" '"tracks"'
test_endpoint "listens-stats"   "http://localhost:8092/listens/stats/daily?days=7" '"data"'
test_post     "smart-search"    "http://localhost:8080/search/smart" '{"query":"комедия"}' '"count"'
test_endpoint "env-editor"      "http://localhost:8083/env" '"vars"'
test_endpoint "duplicates"      "http://localhost:8091/duplicates" '"duplicates"'
test_endpoint "sponsorblock"    "http://localhost:8089/sponsorblock?video_id=dQw4w9WgXcQ" '"segments"'
test_endpoint "oom-check"       "http://localhost:8083/oom-check" '"lines"'
test_endpoint "update-status"   "http://localhost:8084/update/status" '"updates_available"'
test_endpoint "health-memory"   "http://localhost:8084/health/memory" '"services"'
test_endpoint "pwa-music"       "http://localhost:8090/?pwa=music" 'pwa-player'

# --- Security-регрессии (path traversal / SSRF / host-exec) ---
# ВНИМАНИЕ: /notify/update/run и /notify/update/rollback НЕ вызываются здесь
# намеренно: с валидным токеном они реально запускают откат/обновление стека.
GW_TOKEN=$(grep -E '^NOTIFY_GATEWAY_TOKEN=' .env 2>/dev/null | head -1 | cut -d= -f2-)
test_post "sec-no-traversal"  "http://localhost:8087/download" '{"id":"123","format":"../../../../tmp/pwned"}' 'invalid format'
test_post "sec-no-ssrf"        "http://localhost:8087/download" '{"id":"http://169.254.169.254/latest/meta-data/","format":"epub"}' 'not an allowed mirror'
test_post_code "sec-host-exec-no-token" "http://localhost:8083/update/run" "403"
test_post_code "sec-rollback-no-token"  "http://localhost:8083/update/rollback" "403"
test_post_code "sec-env-post-no-token"  "http://localhost:8083/env" "403"
if [ -n "$GW_TOKEN" ]; then
    # С токеном запрос доходит до валидации тела (400 = токен принят, но body пустой).
    test_post_code_auth "sec-env-post-with-token" "http://localhost:8083/env" "$GW_TOKEN" "400"
else
    echo "⚠️  NOTIFY_GATEWAY_TOKEN не найден в .env — проверка с токеном пропущена"
fi

# ============================================================
# E2E-сценарии: реальная логика, а не только /health.
# Требуют запущенный стек и настроенные NAS/Prowlarr/Flibusta/MPD.
# ============================================================

echo "=== E2E: Поиск торрентов ==="
TORRENT_SEARCH=$(curl -sS --max-time 60 "http://localhost:8088/search?q=ubuntu&limit=1" 2>/dev/null)
if echo "$TORRENT_SEARCH" | jq -e '.results | length > 0' >/dev/null 2>&1; then
    echo "  ✅ Поиск торрентов: OK"
else
    echo "  ❌ Поиск торрентов: FAIL $(echo "$TORRENT_SEARCH" | head -c 500)"
    FAIL=$((FAIL+1))
fi

echo "=== E2E: Медиатека (фильмы) ==="
MEDIA_RESULT=$(curl -sS --max-time 30 "http://localhost:8091/movies?limit=1" 2>/dev/null)
if echo "$MEDIA_RESULT" | jq -e '(.items // .movies // []) | length > 0' >/dev/null 2>&1; then
    echo "  ✅ Медиатека: OK"
else
    echo "  ❌ Медиатека: FAIL $(echo "$MEDIA_RESULT" | head -c 500)"
    FAIL=$((FAIL+1))
fi

echo "=== E2E: Музыка (MPD/браузер) ==="
MUSIC_STATUS=$(curl -sS --max-time 15 "http://localhost:8092/status" 2>/dev/null)
MUSIC_MODE=$(echo "$MUSIC_STATUS" | jq -r '.mode // empty' 2>/dev/null)
if echo "$MUSIC_STATUS" | jq -e '(.mpd == "connected") or (.mode == "browser")' >/dev/null 2>&1; then
    echo "  ✅ Музыка: OK (${MUSIC_MODE:-mpd})"
else
    echo "  ❌ Музыка: FAIL $(echo "$MUSIC_STATUS" | head -c 500)"
    FAIL=$((FAIL+1))
fi

echo "=== E2E: Книги (поиск) ==="
BOOK_RESULT=$(curl -sS --max-time 60 --get --data-urlencode 'q=Пушкин' "http://localhost:8087/search" 2>/dev/null)
if echo "$BOOK_RESULT" | jq -e '.books | length > 0' >/dev/null 2>&1; then
    echo "  ✅ Книги: OK"
else
    echo "  ❌ Книги: FAIL $(echo "$BOOK_RESULT" | head -c 500)"
    FAIL=$((FAIL+1))
fi

echo "=== E2E: Читалка (FB2/EPUB) ==="
# Сначала локально скачанная книга (детерминированно, без сети), затем — id из поиска.
BOOK_NAME=$(curl -sS --max-time 10 "http://localhost:8087/downloaded" 2>/dev/null | jq -r '.books[0].name // empty' 2>/dev/null)
if [ -n "$BOOK_NAME" ]; then
    BOOK_ID=$(jq -rn --arg v "$BOOK_NAME" '$v|@uri')
else
    BOOK_ID=$(echo "$BOOK_RESULT" | jq -r '.books[0].id // empty' 2>/dev/null)
fi
if [ -n "$BOOK_ID" ]; then
    BOOK_CODE=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 60 "http://localhost:8087/read/$BOOK_ID" 2>/dev/null)
    if [ "$BOOK_CODE" = "200" ]; then
        echo "  ✅ Читалка: OK (${BOOK_NAME:-$BOOK_ID})"
    else
        echo "  ❌ Читалка: FAIL ($BOOK_CODE)"
        FAIL=$((FAIL+1))
    fi
else
    echo "  ❌ Читалка: FAIL (нет локальной книги и id из поиска)"
    FAIL=$((FAIL+1))
fi

echo "=== E2E: Фото (кэш) ==="
PHOTO_STATUS=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 15 "http://localhost:8091/photos/thumbnail?path=test.jpg" 2>/dev/null)
if [ "$PHOTO_STATUS" = "200" ] || [ "$PHOTO_STATUS" = "404" ]; then
    echo "  ✅ Фото: OK ($PHOTO_STATUS)"
else
    echo "  ❌ Фото: FAIL ($PHOTO_STATUS)"
    FAIL=$((FAIL+1))
fi

echo ""
if [ "$FAIL" -eq 0 ]; then
    echo "✅ Все тесты прошли"
    exit 0
else
    echo "❌ Провалено: $FAIL"
    exit 1
fi
