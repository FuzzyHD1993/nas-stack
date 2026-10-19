#!/usr/bin/env bash
# ============================================================
# nas-stack — универсальный installer
# Synology, QNAP, TrueNAS SCALE, Unraid, OpenMediaVault,
# Linux (Ubuntu/Debian/Arch/Fedora), macOS, Windows (WSL2)
# ============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
cd "$REPO_DIR"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'
log_info()    { echo -e "${BLUE}ℹ${NC}  $*"; }
log_success() { echo -e "${GREEN}✅${NC} $*"; }
log_warn()    { echo -e "${YELLOW}⚠${NC}  $*"; }
log_error()   { echo -e "${RED}❌${NC} $*"; }
log_step()    { echo -e "\n${CYAN}▶${NC} ${CYAN}$*${NC}"; }

DRY_RUN="false"
if [ "${1:-}" = "--dry-run" ]; then DRY_RUN="true"; fi

OS=""; NAS=""; RUNTIME=""; COMPOSE_CMD=""; PROFILE=""
HAS_SYSTEMD="false"; LIBRARY_ROOT=""; PUID=""; PGID=""; SOCKET=""

detect_os() {
    log_step "Определяю ОС..."
    case "$(uname -s)" in
        Linux*)  OS="linux" ;;
        Darwin*) OS="macos" ;;
        *) log_error "Неподдерживаемая ОС: $(uname -s)"; exit 1 ;;
    esac
    log_success "OS: $OS"
}

detect_env() {
    log_step "Определяю NAS и runtime..."
    local info
    info=$(bash scripts/detect-nas.sh)
    NAS=$(echo "$info" | sed -n 's/^NAS_TYPE=//p')
    LIBRARY_ROOT=$(echo "$info" | sed -n 's/^DEFAULT_PATH=//p')
    RUNTIME=$(echo "$info" | sed -n 's/^CONTAINER_RUNTIME=//p')
    SOCKET=$(echo "$info" | sed -n 's/^CONTAINER_SOCKET=//p')
    HAS_SYSTEMD=$(echo "$info" | sed -n 's/^HAS_SYSTEMD=//p')
    PUID=$(echo "$info" | sed -n 's/^USER_UID=//p')
    PGID=$(echo "$info" | sed -n 's/^USER_GID=//p')
    log_success "NAS: $NAS (путь: $LIBRARY_ROOT)"
    log_success "Runtime: $RUNTIME | systemd: $HAS_SYSTEMD"
}

check_requirements() {
    log_step "Проверяю требования..."
    if [ "$RUNTIME" = "none" ]; then
        log_error "Не найден podman или docker"
        echo ""
        case "$NAS" in
            synology)       echo "  Установите Container Manager (App Center)" ;;
            qnap)           echo "  Установите Container Station (App Center)" ;;
            truenas)        echo "  Docker встроен в SCALE 24.04+" ;;
            unraid)         echo "  Docker встроен в Unraid" ;;
            openmediavault) echo "  apt install docker.io docker-compose-plugin" ;;
            macos)          echo "  Docker Desktop: https://docker.com" ;;
            wsl2)           echo "  Docker Desktop для Windows (WSL2 backend)" ;;
            *)              echo "  apt/pacman/dnf install podman" ;;
        esac
        exit 1
    fi
    if [ "$RUNTIME" = "podman" ]; then COMPOSE_CMD="podman compose"; else COMPOSE_CMD="docker compose"; fi

    if [ "$OS" = "linux" ]; then
        local mem
        mem=$(awk '/^MemTotal/{printf "%d", $2/1024/1024}' /proc/meminfo)
        if [ "${mem:-0}" -lt 4 ]; then log_warn "RAM ${mem} GB (< 4 GB)"; fi
    fi
    local disk
    disk=$(df -BG "$REPO_DIR" 2>/dev/null | awk 'NR==2 {gsub("G","",$4); print $4}')
    if [ -n "${disk:-}" ] && [ "$disk" -lt 20 ]; then log_warn "Диск ${disk} GB свободно (< 20 GB)"; fi

    local busy=0
    for port in 8080 8090; do
        if command -v ss >/dev/null 2>&1 && ss -tln 2>/dev/null | grep -q ":$port "; then
            log_warn "Порт $port занят"; busy=$((busy+1))
        fi
    done
    if [ "$busy" -eq 0 ]; then log_success "Порты 8080/8090 свободны"; fi
}

setup_env() {
    log_step "Настраиваю .env..."
    if [ "$DRY_RUN" = "true" ]; then
        log_info "dry-run: пропускаю запись .env"
        return
    fi
    if [ ! -f .env ]; then
        cp .env.example .env
        sed -i "s|^LIBRARY_ROOT=.*|LIBRARY_ROOT=$LIBRARY_ROOT|" .env 2>/dev/null || echo "LIBRARY_ROOT=$LIBRARY_ROOT" >> .env
        log_success ".env создан (LIBRARY_ROOT=$LIBRARY_ROOT)"
    else
        cp .env ".env.bak.$(date +%Y%m%d-%H%M%S)"
        log_info ".env уже есть — LIBRARY_ROOT не меняю (бэкап создан)"
    fi
    # Служебные ключи для wizard/профиля (идемпотентно)
    set_kv() {
        if grep -q "^$1=" .env; then sed -i "s|^$1=.*|$1=$2|" .env; else echo "$1=$2" >> .env; fi
    }
    set_kv NAS_TYPE "$NAS"
    set_kv CONTAINER_RUNTIME "$RUNTIME"
    set_kv HAS_SYSTEMD "$HAS_SYSTEMD"
    set_kv PUID "$PUID"
    set_kv PGID "$PGID"
    log_success ".env готов"
}

setup_demons() {
    log_step "Хост-демоны..."
    if [ "$DRY_RUN" = "true" ]; then
        log_info "dry-run: systemd/демоны не трогаю"
        return
    fi
    if [ "$HAS_SYSTEMD" = "true" ] && [ -d systemd ]; then
        mkdir -p ~/.config/systemd/user/
        cp systemd/*.service ~/.config/systemd/user/ 2>/dev/null || true
        systemctl --user daemon-reload
        for unit in nas-notify.service nas-stack-watchdog.timer; do
            if [ -f ~/.config/systemd/user/$unit ]; then
                systemctl --user enable "$unit" 2>/dev/null || true
                systemctl --user start "$unit" 2>/dev/null || true
                log_success "$unit включён"
            fi
        done
    else
        PROFILE="--profile nas-non-systemd"
        log_warn "systemd нет — демоны как контейнеры ($PROFILE)"
        log_info "host-agent/books-opener — отдельные проекты, запускаются отдельно"
    fi
}

setup_mpd() {
    log_step "Проверяю MPD (Hi-Res аудио)..."

    # Ищем реальный путь к музыке: .env → LIBRARY_ROOT → типовые пути.
    local mpd_music=""
    if [ -f .env ]; then
        local env_music
        env_music=$(grep -E '^MUSIC_HOST_DIR=' .env 2>/dev/null | tail -1 | cut -d= -f2- || true)
        [ -n "${env_music:-}" ] && mpd_music="$env_music"
    fi
    if [ -z "$mpd_music" ]; then
        for cand in "$LIBRARY_ROOT/Music" "/mnt/nas/root/FuzzyHD/Music" "$HOME/Music"; do
            if [ -d "$cand" ]; then mpd_music="$cand"; break; fi
        done
    fi
    [ -z "$mpd_music" ] && mpd_music="$LIBRARY_ROOT/Music"
    log_info "Музыка: $mpd_music"

    if [ "$DRY_RUN" = "true" ]; then
        log_info "dry-run: установка и настройка MPD пропущены"
        return
    fi

    if command -v mpd >/dev/null 2>&1; then
        log_success "MPD уже установлен"
    else
        case "$OS" in
            linux)
                log_info "Устанавливаю MPD..."
                if command -v apt >/dev/null 2>&1; then
                    sudo apt install -y mpd 2>&1 | tail -3 || true
                elif command -v pacman >/dev/null 2>&1; then
                    sudo pacman -S --noconfirm mpd 2>&1 | tail -3 || true
                elif command -v dnf >/dev/null 2>&1; then
                    sudo dnf install -y mpd 2>&1 | tail -3 || true
                elif command -v zypper >/dev/null 2>&1; then
                    sudo zypper install -y mpd 2>&1 | tail -3 || true
                else
                    log_warn "Неизвестный пакетный менеджер — MPD не установлен"
                fi
                ;;
            macos)
                if command -v brew >/dev/null 2>&1; then
                    log_info "Устанавливаю MPD через brew..."
                    brew install mpd 2>&1 | tail -3 || true
                else
                    log_warn "Homebrew не найден — MPD не установлен"
                fi
                ;;
        esac
    fi

    if ! command -v mpd >/dev/null 2>&1; then
        log_warn "MPD не установлен — музыка будет в браузерном режиме (HTML5 + ffmpeg)"
        return
    fi

    local conf_dir="$HOME/.config/mpd"
    local conf="$conf_dir/mpd.conf"
    if [ ! -f "$conf" ]; then
        log_info "Генерирую mpd.conf..."
        mkdir -p "$conf_dir/playlists"
        local audio_output
        if [ "$OS" = "macos" ]; then
            audio_output='audio_output {
    type "osx"
    name "CoreAudio"
    mixer_type "software"
}'
        elif command -v pipewire >/dev/null 2>&1; then
            audio_output='audio_output {
    type "pipewire"
    name "PipeWire"
}'
        elif command -v pulseaudio >/dev/null 2>&1; then
            audio_output='audio_output {
    type "pulse"
    name "PulseAudio"
    mixer_type "software"
}'
        else
            audio_output='audio_output {
    type "alsa"
    name "ALSA"
    mixer_type "software"
}'
        fi
        cat > "$conf" << MPDEOL
music_directory     "$mpd_music"
playlist_directory  "$conf_dir/playlists"
db_file             "$conf_dir/database"
log_file            "$conf_dir/log"
pid_file            "$conf_dir/pid"
state_file          "$conf_dir/state"
auto_update         "yes"

bind_to_address     "0.0.0.0"
port                "6600"

$audio_output

decoder {
    plugin "flac"
    enabled "yes"
}
MPDEOL
        log_success "mpd.conf создан: $conf"
    else
        log_info "mpd.conf уже существует — не трогаю"
    fi

    log_info "Запускаю MPD..."
    if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
        systemctl --user enable --now mpd 2>/dev/null || true
    fi
    if ! (exec 3<>/dev/tcp/127.0.0.1/6600) 2>/dev/null; then
        mpd "$conf" 2>/dev/null || true
    fi
    sleep 2

    if (exec 3<>/dev/tcp/127.0.0.1/6600) 2>/dev/null; then
        log_success "MPD работает на :6600"
    else
        log_warn "MPD не отвечает на :6600 — проверьте $conf"
    fi
}

start_stack() {
    log_step "Запускаю стек ($COMPOSE_CMD $PROFILE)..."
    if [ "$DRY_RUN" = "true" ]; then
        log_info "dry-run: $COMPOSE_CMD $PROFILE up -d --build"
        return
    fi
    $COMPOSE_CMD $PROFILE up -d --build || { log_error "up failed"; exit 1; }
    log_success "Стек запущен"
}

wait_healthy() {
    if [ "$DRY_RUN" = "true" ]; then return; fi
    log_step "Жду gateway..."
    local waited=0
    while [ $waited -lt 120 ]; do
        curl -sS --max-time 2 http://localhost:8080/health >/dev/null 2>&1 && { log_success "Gateway отвечает"; return; }
        sleep 2; waited=$((waited+2))
    done
    log_warn "Gateway не ответил за 120s — проверьте логи"
}

next_steps() {
    echo ""
    echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${GREEN}✅ Установка завершена${NC}"
    echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    echo -e "${CYAN}🌐 Откройте:${NC} http://localhost:8090/setup"
    echo "   1. Пройдите wizard (5 шагов)"
    echo "   2. Перезапустите: $COMPOSE_CMD up -d --force-recreate"
    echo ""
    echo -e "${CYAN}📊 Окружение:${NC} OS=$OS NAS=$NAS RUNTIME=$RUNTIME SYSTEMD=$HAS_SYSTEMD"
    echo "   LIBRARY_ROOT=$LIBRARY_ROOT | PUID=$PUID PGID=$PGID"
}

main() {
    detect_os
    detect_env
    check_requirements
    setup_env
    setup_mpd
    setup_demons
    start_stack
    wait_healthy
    next_steps
}

main "$@"
