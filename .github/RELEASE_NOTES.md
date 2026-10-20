# NAS Stack v1.5.2

Релиз инфраструктурных фиксов rootless-Podman. Метка `v1.5.2` перевыпущена:
в прежней версии был неполный набор правок.

## 🐞 Исправлено

- **systemd больше не убивает conmon'ы контейнеров.** `nas-stack.service` и
  `nas-stack-watchdog.service` получили `KillMode=process`: раньше при
  остановке oneshot-юнита systemd делал `SIGKILL` дочерним conmon'ам, и
  контейнеры оставались «Up» без init-процесса (healthcheck → exit 1,
  `podman exec` → *cannot exec in a stopped container*).
- **Сеть стека на rootless Podman.** Rootless netns, созданный до готовности
  сети хоста, оставался без IPv4-маршрута: контейнеры теряли DNS и интернет и
  не видели хост (`host.containers.internal` → notify-service `:8083`).
  Новый `scripts/ensure-rootless-net.sh` проверяет и чинит netns; его зовут
  `scripts/start-stack.sh` (пре-флайт) и `scripts/watchdog.sh` (каждые 5 минут).
- **`scripts/start-stack.sh` больше не считает упавшими `auth-service`
  (`:8086`), `media-service` (`:8091`) и `music-service` (`:8092`)** — они
  намеренно не публикуются на хост, и проверка через `127.0.0.1` давала ложный
  «упавший сервис», из-за чего юнит старта всегда падал с `exit 1`.
- **`notify-service` явно слушает `0.0.0.0:8083`.**
- **MPD виден из `music-service`** (`MPD_ADDR=host.containers.internal:6600`).

---

# NAS Stack v1.0.0-beta

Первая публичная бета self-hosted платформы для домашнего NAS.

## ✨ Основное

- **Setup Wizard** `/setup` — 5 шагов: пути → сеть → API-ключи → проверка → готово.
  Автоопределение NAS (Synology/QNAP/TrueNAS/Unraid/OMV/Linux/macOS/WSL2),
  подсказки путей, генератор CIFS/NFS fstab, мерж `.env` с бэкапом.
- **Универсальная установка**: `scripts/install.sh` (runtime podman/docker,
  systemd или Docker-профиль `nas-non-systemd`), `update.sh`, `uninstall.sh`.
- **Единый дашборд**: торренты, медиа (TMDb, транскод), музыка (MPD, умные
  плейлисты, история), книги (Флибуста + EPUB-читалка), YouTube (yt-dlp,
  SponsorBlock), фото, мониторинг (Prometheus/Grafana), здоровье сервисов.
- **Хост-демоны в монорепо**: `host-agent` (sensors/SMART/cleanup/дубликаты)
  и `books-opener`; собираются в Docker и GoReleaser.

## 🐳 Образы (GHCR)

15 сервисов, `linux/amd64` + `linux/arm64`:
`gateway`, `auth-service`, `stats-service`, `torrent-service`,
`scheduler-service`, `history-service`, `books-service`, `search-service`,
`youtube-service`, `web-dashboard`, `media-service`, `music-service`,
`notify-service`, `host-agent`, `books-opener`.

## 🛠 Бинарники

GoReleaser: `nas-notify`, `nas-host-agent`, `nas-books-opener`
(linux amd64/arm64; notify также darwin/windows).

## 🐞 Исправлено

- Утечка горутин web-dashboard: 462 → 16 (общий HTTP-клиент + `resp.Close()`).
- Обложки альбомов: 30+ имён, рекурсия в `Scans/`, metaflac/ffmpeg fallback.
- Fiber v3 zero-copy в Prometheus labels (`strings.Clone`).
- Экран логина при `AUTH_ENABLED=false`.
- Мини-плеер в шапке работает с любой вкладки; прогресс-бары эпизодов.
- NAS-маунт: `nofail,_netdev,x-systemd.automount` + перемонтирование в watchdog.

## 📦 Пакеты

- `.deb` (Linux), `.exe` (Windows, Inno Setup), `.dmg` (macOS) — workflow
  `build-installers.yml`.
- Подпись: GPG для `.deb` (опционально), SignPath для Windows (OSS), macOS —
  без notarization (right-click → Open).

## ⚠️ Известные ограничения

- Реальная проверка на QNAP/TrueNAS/Unraid/Windows не проводилась.
- `books-opener` в Docker не открывает GUI — на NAS не нужен.
- macOS notarization / Windows EV-сертификат не настроены.

**Full Changelog**: см. `CHANGELOG.md`.
