#!/bin/bash
# update-stack.sh — безопасное обновление стека с авто-откатом.
set -e
REPO_DIR="$(cd "$(dirname "$(readlink -f "$0")")/.." && pwd)"
cd "$REPO_DIR"

BEFORE=$(git rev-parse HEAD)
echo "📌 Before: $BEFORE"

# Снапшот btrfs (если есть snapper)
if command -v snapper &>/dev/null; then
    sudo -n snapper -c root create --description "pre-update $(date +%Y%m%d-%H%M%S)" || true
fi

git pull github main || { echo "❌ pull"; exit 1; }
podman compose build || { echo "❌ build"; git reset --hard "$BEFORE"; exit 1; }
podman compose up -d || { echo "❌ up"; git reset --hard "$BEFORE"; exit 1; }

sleep 30
FAILED=0
declare -A PORTS=(
    [gateway]=8080 [stats-service]=8082 [torrent-service]=8081
    [scheduler-service]=8084 [history-service]=8085 [auth-service]=8086
    [books-service]=8087 [search-service]=8088 [youtube-service]=8089
    [web-dashboard]=8090 [media-service]=8091 [music-service]=8092
)
for svc in "${!PORTS[@]}"; do
    if ! curl -sf "http://localhost:${PORTS[$svc]}/health" > /dev/null; then
        echo "❌ $svc failed"
        FAILED=$((FAILED+1))
    fi
done

if [ "$FAILED" -gt 0 ]; then
    echo "❌ $FAILED failed — откат"
    git reset --hard "$BEFORE"
    podman compose build
    podman compose up -d
    exit 1
fi

echo "✅ Обновление успешно"
git log --oneline -1
