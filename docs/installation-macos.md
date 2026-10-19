# Установка на macOS

1. Установите [Docker Desktop](https://www.docker.com/products/docker-desktop/) (podman также работает через `podman machine`).
2. `git clone <repo> && cd nas-stack`.
3. `cp .env.example .env`.
4. `docker compose up -d` (или `podman compose up -d`).
5. Откройте `http://localhost:8090/setup` и пройдите wizard.

Ограничения: пути вида `/mnt/nas` замените на `/Volumes/nas` (или свой путь) в wizard.
