#!/usr/bin/env bash
set -e

# Устанавливает systemd user-юнит nas-host-agent в ~/.config/systemd/user/
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

mkdir -p ~/.config/systemd/user/
cp "$REPO_DIR"/systemd/*.service ~/.config/systemd/user/
systemctl --user daemon-reload

echo "✅ Юнит установлен:"
ls -la ~/.config/systemd/user/nas-host-agent.service
