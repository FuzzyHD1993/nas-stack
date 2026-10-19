#!/bin/bash
# btrfs-snapshot.sh — снапшот через snapper (если установлен).
if command -v snapper &>/dev/null; then
    sudo -n snapper -c root create --description "manual $(date +%Y%m%d-%H%M%S)"
else
    echo "snapper не установлен — пропуск (pacman -S snapper)"
fi
