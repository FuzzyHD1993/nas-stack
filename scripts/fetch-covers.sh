#!/usr/bin/env bash
# fetch-covers.sh — докачивает обложки для альбомов без локальных изображений.
# Использует теги FLAC/MP3 (ffprobe) и iTunes Search API.
#
# Использование:
#   scripts/fetch-covers.sh [--dry-run] [--limit N]
#
# Переменные:
#   MUSIC_DIR  — корень музыки (по умолчанию /mnt/nas/root/FuzzyHD/Music)
set -euo pipefail

MUSIC_DIR="${MUSIC_DIR:-/mnt/nas/root/FuzzyHD/Music}"
LIMIT=0
DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) DRY_RUN=1 ;;
        --limit)   LIMIT="${2:-0}"; shift ;;
        -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
        *) echo "Неизвестный аргумент: $1" >&2; exit 1 ;;
    esac
    shift
done

for cmd in curl jq ffprobe; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "❌ Нужен $cmd" >&2; exit 1; }
done
[ -d "$MUSIC_DIR" ] || { echo "❌ Нет каталога: $MUSIC_DIR" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

audio_find() {
    find "$1" -maxdepth 1 -type f \( \
        -iname '*.flac' -o -iname '*.mp3' -o -iname '*.m4a' -o -iname '*.aac' \
        -o -iname '*.ogg' -o -iname '*.opus' -o -iname '*.wav' -o -iname '*.ape' \
        -o -iname '*.wv' -o -iname '*.dsf' -o -iname '*.dff' \) -print
}

# Чистим годы, лейблы, «Дискография», CD1 и т.п. — как normalizeForSearch в music-service.
norm() {
    sed -E \
        -e 's/\([0-9]{4}(-[0-9]{4})?\)//g' \
        -e 's/\[[^]]*\]//g' \
        -e 's/\{[^}]*\}//g' \
        -e 's/\([^)]*\)//g' \
        -e 's/[[:space:]]*[-–—][[:space:]]*(Дискография|Discography|FLAC|MP3|Lossless|Collection).*//Ig' \
        -e 's/\b(19|20)[0-9]{2}\b//g' \
        -e 's/\b(CD|Disc)[0-9]+\b//Ig' \
        -e 's/_/ /g' \
        -e 's/[[:space:]]+/ /g' \
        -e 's/^[[:space:]]+//; s/[[:space:]]+$//'
}

is_structural() {
    case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
        albums|singles|compilations|live|soundtracks|discography|remastered|deluxe|bonus|cd*|disc*|_*) return 0 ;;
        *) return 1 ;;
    esac
}

# Каталоги с аудио и каталоги, где уже есть хоть одно изображение.
find "$MUSIC_DIR" -type f \( \
    -iname '*.flac' -o -iname '*.mp3' -o -iname '*.m4a' -o -iname '*.aac' \
    -o -iname '*.ogg' -o -iname '*.opus' -o -iname '*.wav' -o -iname '*.ape' \
    -o -iname '*.wv' -o -iname '*.dsf' -o -iname '*.dff' \) -printf '%h\n' \
    | sort -u > "$TMP/albums"
find "$MUSIC_DIR" -type f \( \
    -iname '*.jpg' -o -iname '*.jpeg' -o -iname '*.png' -o -iname '*.webp' \
    -o -iname '*.bmp' -o -iname '*.gif' \) -printf '%h\n' | sort -u > "$TMP/withimg"
comm -23 "$TMP/albums" "$TMP/withimg" > "$TMP/missing"

total=$(wc -l < "$TMP/missing" | tr -d ' ')
echo "🎵 MUSIC_DIR=$MUSIC_DIR"
echo "📀 Альбомов без изображений: $total"
[ "$LIMIT" -gt 0 ] && echo "⏱  Лимит: $LIMIT"
[ "$DRY_RUN" -eq 1 ] && echo "🧪 dry-run: файлы не пишутся"

processed=0; found=0; missed=0; skipped=0

while IFS= read -r dir; do
    [ -n "$dir" ] || continue
    if [ "$LIMIT" -gt 0 ] && [ "$processed" -ge "$LIMIT" ]; then break; fi

    first="$(audio_find "$dir" | head -1 || true)"
    if [ -z "$first" ]; then skipped=$((skipped+1)); continue; fi
    processed=$((processed+1))

    artist="$(ffprobe -v error -show_entries format_tags=artist -of default=noprint_wrappers=1:nokey=1 "$first" 2>/dev/null | head -1 || true)"
    if [ -z "$artist" ]; then
        artist="$(ffprobe -v error -show_entries format_tags=album_artist -of default=noprint_wrappers=1:nokey=1 "$first" 2>/dev/null | head -1 || true)"
    fi
    album="$(ffprobe -v error -show_entries format_tags=album -of default=noprint_wrappers=1:nokey=1 "$first" 2>/dev/null | head -1 || true)"

    if is_structural "$artist"; then artist=""; fi

    rel="${dir#"$MUSIC_DIR"/}"
    top="${rel%%/*}"
    topalbum=""
    if [[ "$top" =~ ^(.+)[[:space:]]-[[:space:]][0-9]{4}[[:space:]]-[[:space:]](.+)$ ]]; then
        topartist="${BASH_REMATCH[1]}"
        topalbum="${BASH_REMATCH[2]}"
    else
        topartist="$top"
    fi
    if [ -z "$artist" ]; then
        artist="$(printf '%s' "$topartist" | norm)"
    fi
    if [ -z "$album" ]; then
        base="$(basename "$dir")"
        if is_structural "$base"; then
            base="$(basename "$(dirname "$dir")")"
        fi
        if [ -n "$topalbum" ] && [ "$base" = "$top" ]; then
            base="$topalbum"
        fi
        if [ -n "$artist" ] && [[ "$base" == "$artist - "* ]]; then
            base="${base#"$artist - "}"
        fi
        base="$(printf '%s' "$base" | sed -E 's/^[0-9]{4}[[:space:]]*-[[:space:]]*//')"
        album="$(printf '%s' "$base" | norm)"
    fi
    artist="$(printf '%s' "$artist" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
    album="$(printf '%s' "$album" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
    if [ -z "$artist" ] || [ -z "$album" ]; then
        echo "⚠️ Не удалось определить artist/album: $dir"
        skipped=$((skipped+1))
        continue
    fi

    term="$artist $album"
    resp="$(curl -sS --max-time 15 --get \
        --data-urlencode "term=$term" \
        --data-urlencode "entity=album" \
        --data-urlencode "limit=1" \
        "https://itunes.apple.com/search" 2>/dev/null || true)"
    url="$(printf '%s' "$resp" | jq -r '.results[0].artworkUrl100 // empty' 2>/dev/null | sed 's/100x100bb/600x600bb/; s/100x100-75/600x600-75/' || true)"

    if [ -z "$url" ] || [ "$url" = "null" ]; then
        echo "❌ Не найдено: $artist — $album"
        missed=$((missed+1))
        continue
    fi

    out="$dir/cover.jpg"
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "🔍 [dry] $artist — $album → $url"
        found=$((found+1))
        continue
    fi

    tmpout="$out.tmp.$$"
    if curl -fsSL --max-time 30 -o "$tmpout" "$url" 2>/dev/null \
        && [ "$(stat -c%s "$tmpout" 2>/dev/null || echo 0)" -gt 1024 ]; then
        mv "$tmpout" "$out"
        echo "✅ $artist — $album → $out"
        found=$((found+1))
    else
        rm -f "$tmpout"
        echo "❌ Ошибка загрузки: $artist — $album"
        missed=$((missed+1))
    fi
    sleep 0.25
done < "$TMP/missing"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Обработано: $processed | Скачано: $found | Не найдено: $missed | Пропущено: $skipped"
