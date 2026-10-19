# Установка на ARM (Raspberry Pi и др.)

Образы собираются под `linux/arm64` (workflow `publish.yml`).

1. 64-битная ОС (Raspberry Pi OS 64-bit / Ubuntu ARM64).
2. Установите podman + compose-провайдер.
3. `git clone <repo> && cd nas-stack && cp .env.example .env`.
4. `podman compose up -d --build` (сборка на устройстве может быть долгой).
5. `http://<pi-ip>:8090/setup`.

Рекомендуется ≥4 ГБ RAM; media-service (ffmpeg-транскод) на ARM может быть медленным.
