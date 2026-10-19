#!/usr/bin/env bash
set -e

# Устанавливает systemd user-юниты nas-stack в ~/.config/systemd/user/
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

mkdir -p ~/.config/systemd/user/
cp "$REPO_DIR"/systemd/*.service ~/.config/systemd/user/
cp "$REPO_DIR"/systemd/*.timer ~/.config/systemd/user/ 2>/dev/null || true
systemctl --user daemon-reload

echo "✅ Юниты установлены:"
ls -la ~/.config/systemd/user/nas-*.service
