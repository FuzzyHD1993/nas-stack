# Changelog

## [1.5.2] — 2026-10-21

Релиз перевыпущен: в версию, помеченную `v1.5.2`, попала неполная
набор правок инфраструктуры. Текущий `v1.5.2` содержит всё нижеперечисленное.

### Fixed
- **systemd больше не убивает conmon'ы контейнеров.** Юниты `nas-stack.service`
  и `nas-stack-watchdog.service` — `Type=oneshot` с дефолтным
  `KillMode=control-group`: при остановке systemd делал `SIGKILL` дочерним
  conmon'ам, и контейнеры оставались в состоянии «Up» без init-процесса
  (`podman exec` → *cannot exec in a stopped container*, healthcheck → exit 1).
  Добавлен `KillMode=process`; watchdog'у — `TimeoutStartSec=600`, чтобы
  длинный `podman restart` не прерывался по дефолтному 90-секундному таймауту.
- **Восстановлена сеть стека на rootless Podman.** Rootless netns, созданный
  на старте ОС (до готовности сети хоста), оставался без IPv4-маршрута по
  умолчанию: контейнеры теряли DNS и интернет и не видели хост
  (`host.containers.internal` → notify-service `:8083` недоступен, MPD `:6600`
  недоступен, books-service уходил на онлайн-зеркала). Добавлен
  `scripts/ensure-rootless-net.sh`: проверка и пересоздание netns; вызывается
  из `scripts/start-stack.sh` (пре-флайт) и `scripts/watchdog.sh` (авто-починка
  каждые 5 минут).
- **`scripts/start-stack.sh` больше не считает упавшими сервисы, которые не
  публикуются на хост.** `auth-service` (`:8086`), `media-service` (`:8091`) и
  `music-service` (`:8092`) доступны только изнутри сети стека; проверка через
  `127.0.0.1` на хосте давала ложный «упавший сервис» → юнит `nas-stack.service`
  всегда завершался с `exit 1` и постоянно пересоздавал эти контейнеры.
  Теперь их `/health` проверяется изнутри контейнера.
- **`notify-service` явно слушает `0.0.0.0:8083`** (вместо подразумеваемого
  `:8083`): контейнеры стека ходят к нему через `host.containers.internal`.
- **MPD снова виден из `music-service`**: `MPD_ADDR` в `.env` заменён с
  `192.168.0.4:6600` (LAN-IP хоста, недостижим из контейнера) на
  `host.containers.internal:6600` — в `/health` теперь `"mpd":"connected"`.

### Changed
- `nas-stack.service` и `nas-stack-watchdog.service` больше не убивают
  conmon'ы при остановке; watchdog-таймер по-прежнему работает каждые 5 минут
  и больше не падает с `Failed with result 'timeout'`.

## [1.5.1] — 2026-10-20

- Защита `torrent-service` от SSRF (`torrent-service/ssrf.go` + тесты).
- Правки UI дашборда: книги, музыка, торренты; метрики `youtube-service`.
- Обновления по 17 сервисам стека.

## [1.5.0] — 2026-10-19

- Усиление gateway: прокси, метрики, авторизация admin-эндпоинтов
  (`gateway/hardening_test.go`, `gateway/admin_auth_test.go`).
- Единый `/health` для всех сервисов (`internal/platform/health.go`).

## [1.0.0-beta] — 2026-10-17

### Added
- Setup Wizard `/setup` (5 шагов): пути → сеть → API-ключи → проверка → готово.
- `POST /api/setup/check-path`, `POST /api/setup/save` (мерж `.env` + бэкап).
- CLI: `scripts/install.sh`, `update.sh`, `uninstall.sh`.
- CI (GitHub Actions): gofmt/vet/build/test по всем модулям.
- Publish workflow: multi-arch образы (amd64/arm64) в GHCR по тегу.
- Docs: установка Linux/macOS/Windows/Synology/ARM, API, архитектура.
- LICENSE (MIT), CONTRIBUTING, SECURITY, issue templates.

### Fixed
- Утечка горутин web-dashboard (462 → 16): общий HTTP-клиент + `resp.Close()`.
- Обложки альбомов: 30+ вариантов имён, рекурсия, metaflac/ffmpeg.
- Fiber v3 zero-copy в Prometheus labels (`strings.Clone`).
- Экран логина при `AUTH_ENABLED=false`.
- Мини-плеер в шапке работает с любой вкладки.
- NAS-маунт: `nofail,_netdev,x-systemd.automount` + перемонтирование в watchdog.

### Changed
- Все пути/IP/ключи — через `.env` (v10.1).
- Умные плейлисты, история прослушиваний, умный поиск, дубликаты, SponsorBlock.
