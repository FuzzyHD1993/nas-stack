#!/usr/bin/env bash
# detect-nas.sh — определяет тип NAS/ОС, пути, runtime и наличие systemd.

detect_nas() {
    if grep -qi microsoft /proc/version 2>/dev/null; then
        echo "wsl2"
    elif [ -f /etc/synoinfo.conf ] && [ -d /volume1 ]; then
        echo "synology"
    elif [ -f /etc/config/uLinux.conf ] || [ -d /share/CACHEDEV1_DATA ]; then
        echo "qnap"
    elif [ -f /etc/truenas-version ] || grep -q "TrueNAS" /etc/version 2>/dev/null; then
        echo "truenas"
    elif [ -f /etc/unraid-version ]; then
        echo "unraid"
    elif [ -f /etc/openmediavault/config.xml ]; then
        echo "openmediavault"
    elif [ "$(uname -s)" = "Darwin" ]; then
        echo "macos"
    else
        echo "generic-linux"
    fi
}

detect_default_path() {
    case "$1" in
        synology)       echo "/volume1/homes" ;;
        qnap)           echo "/share/CACHEDEV1_DATA/homes" ;;
        truenas)        echo "/mnt" ;;
        unraid)         echo "/mnt/user" ;;
        openmediavault) echo "/srv" ;;
        macos)          echo "/Volumes" ;;
        wsl2)           echo "$HOME/media" ;;
        *)              echo "/mnt" ;;
    esac
}

detect_container_runtime() {
    if command -v podman >/dev/null 2>&1; then
        echo "podman"
    elif command -v docker >/dev/null 2>&1; then
        echo "docker"
    else
        echo "none"
    fi
}

detect_container_socket() {
    case "$(detect_container_runtime)" in
        podman)
            if [ -S "/run/user/$(id -u)/podman/podman.sock" ]; then
                echo "/run/user/$(id -u)/podman/podman.sock"
            else
                echo "/run/podman/podman.sock"
            fi
            ;;
        docker) echo "/var/run/docker.sock" ;;
        *)      echo "" ;;
    esac
}

detect_systemd() {
    if command -v systemctl >/dev/null 2>&1 && systemctl --user status >/dev/null 2>&1; then
        echo "true"
    else
        echo "false"
    fi
}

NAS=$(detect_nas)
PATH_DEF=$(detect_default_path "$NAS")
RUNTIME=$(detect_container_runtime)
SOCKET=$(detect_container_socket)
HAS_SYSTEMD=$(detect_systemd)

cat <<EOF
NAS_TYPE=$NAS
DEFAULT_PATH=$PATH_DEF
CONTAINER_RUNTIME=$RUNTIME
CONTAINER_SOCKET=$SOCKET
HAS_SYSTEMD=$HAS_SYSTEMD
USER_UID=$(id -u)
USER_GID=$(id -g)
EOF
