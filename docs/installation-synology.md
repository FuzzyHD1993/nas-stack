# Установка на Synology (Container Manager / Compose)

1. Включите SSH и Container Manager.
2. Склонируйте репозиторий (или загрузите архив) в `/volume1/docker/nas-stack`.
3. `cp .env.example .env`, задайте `LIBRARY_ROOT=/volume1/...`.
4. `sudo podman compose up -d` (или Docker Compose в Container Manager).
5. Откройте `http://<synology-ip>:8090/setup`.

Учтите: DSM может не поддерживать rootless podman — используйте Docker/Compose из Container Manager.
