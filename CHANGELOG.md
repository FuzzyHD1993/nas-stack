# Changelog

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
