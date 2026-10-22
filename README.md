# 🖥️ NAS-Stack

Домашний сервер «под ключ»: медиацентр, торрент-качалка, музыка на MPD, книги, YouTube,
фото и метрики сервера — всё в одном веб-дашборде. Микросервисная
архитектура на **Go 1.27 + Fiber v3**, **Podman Compose**, **PostgreSQL 17** и **Redis 7**,
с мониторингом через **Prometheus** и **Grafana 11.3**.

![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)
![Fiber](https://img.shields.io/badge/Fiber-v3.5-00ACD7?logo=fiber&logoColor=white)
![Podman](https://img.shields.io/badge/Podman-compose-892CA0?logo=podman&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-17-4169E1?logo=postgresql&logoColor=white)
![Redis](https://img.shields.io/badge/Redis-7-DC382D?logo=redis&logoColor=white)
![Grafana](https://img.shields.io/badge/Grafana-11.3-F46800?logo=grafana&logoColor=white)
![License](https://img.shields.io/badge/License-MIT-green)
![Version](https://img.shields.io/badge/version-1.8.0-blue)

---

## 📑 Содержание

- [Возможности](#-возможности)
- [Архитектура](#-архитектура)
- [Сервисы и порты](#сервисы-и-порты)
- [Быстрый старт](#-быстрый-старт)
- [Запуск стека](#-запуск-стека)
- [Переменные окружения](#-переменные-окружения-env)
- [Маршрутизация gateway](#-маршрутизация-gateway)
- [Планировщик](#-планировщик-scheduler-service)
- [Стабильность и мониторинг](#-стабильность-и-мониторинг)
- [Скрипты](#-скрипты)
- [systemd](#-systemd)
- [Мультирум (Snapcast)](#-мультирум-snapcast--опционально)
- [Тесты](#-тесты)
- [Разработка](#-разработка)
- [Troubleshooting](#-troubleshooting)
- [Лицензия](#-лицензия)

---

## ✨ Возможности

### 📥 Торренты
- Поиск через **Prowlarr** — 10+ трекеров, в т.ч. **русских**: RuTracker.org, Rutor, Byrutor,
  Catorrent, PC-torrent, RinTor.NeT, sosulki, torrent-pirat, xxxtor (+ 1337x, TPB, Knaben, Torrent9, MagnetDownload)
- **RuTracker без VPN** — авто-логин через FlareSolverr + cookie-сессия
- **Определения индексаторов всегда свежие**: `scripts/update-indexer-definitions.sh`
  (daily таймер `nas-prowlarr-definitions`, v11-схема, sha-skip, авто-рестарт Prowlarr при изменениях)
- Управление **Synology DownloadStation**: добавление, пауза, удаление, лимит одновременных загрузок
- **Авто-старт задач**: магнит добавляется с `create_authorized=true` + сразу стартует, id задачи резолвится автоматически
- **Мульти-категории**: поиск и фильтры принимают несколько `categories`/`indexerIds` в одном запросе
- **Авто-определение категории** по имени (Movies / TV / Music / Games / Courses / Books)
- Фильтры, сортировка, статистика, детальная карточка (трекеры, файлы, ETA, рейтинг RuTracker)

### 🎬 Медиа
- Библиотека фильмов и сериалов с постерами и обогащением из **TMDb**
- **HTML5-плеер** с транскодом (AC-3 / DTS / HEVC → H.264 / AAC), субтитрами, PiP
- **Продолжение просмотра** (progress + полка «Продолжить»), коллекции, актёры, похожие, трейлеры
- **Плейлисты фильмов**, рулетка «🎲 Случайный фильм», умные полки
- Авто-сканирование, watchdog новых файлов, авто-переименование, подписки на сериалы

### 🎵 Музыка (MPD)
- Управление **MPD**: play/pause/next/prev, seek, shuffle/repeat, громкость, визуализатор
- Метаданные **FLAC / DSD** (sample rate, битность, битрейт), **bit-perfect** индикатор (`/audio/chain`)
- **Текст песни** (LRCLIB), **избранное**, **рейтинг**, **похожие треки**
- Очередь с drag & drop, «📻 Моя волна», статистика прослушивания
- Плейлисты, в т.ч. **«➕ В плейлист»** из любого списка треков
- Надёжный `next`/`prev`: при одиночном треке в очереди автоматически догружает остальные файлы папки
- Поиск, мини-плеер (обложка/прогресс/громкость), спящий таймер
- **Обложки**: локальные (`cover/folder/front/album/artwork/обложка`, любое фото в папке,
  поиск по всем изданиям) → iTunes → MusicBrainz; докачка: `scripts/fetch-covers.sh`

### 📚 Книги
- Поиск и скачивание с **Флибусты** (FB2 / EPUB / MOBI)
- **Встроенная читалка FB2/EPUB** (foliate-js) прямо в дашборде — темы, шрифт, прогресс

### 📹 YouTube
- Скачивание видео в **4K** через **yt-dlp** (обход проверок через `bgutil-pot`)
- Очередь задач, прогресс, список скачанных файлов

### 🍿 Киноафиша
- TMDb-тренды: сейчас в кино, скоро, популярное, топ, сериалы
- **Wishlist** («хочу посмотреть») + уведомления о релизах (задача `wishlist-release-check`)

### 🖼️ Фото
- Галерея из `/Photos`, группировка по датам, миниатюры (ffmpeg), лайтбокс

### 🚀 v1.1
- **Auto-Update**: баннер в UI + кнопка «Обновить» (git pull → rebuild → health-check → авто-откат через host-agent).
- **DLNA**: minidlna (профиль `dlna`) — телевизоры видят медиатеку.
- **Chromecast**: кнопка «📺 Cast» в HTML5-плеере (Presentation API, Chrome).
- **Мультипрофили**: взрослый/детский/гость, PIN, отдельный JWT с `profile_id`.

### 📊 Метрики и мониторинг
- CPU (в т.ч. по ядрам), RAM, GPU (nvidia-smi), диски, сеть, load, процессы
- **Датчики (lm-sensors)** и **SMART** — через `nas-host-agent` (:9998)
- **Двойные пороги диска** (`DISK_WARN_PERCENT` / `DISK_CRIT_PERCENT`) + `disk-check-detailed`
- **Healthcheck** сервисов, история, **очистка диска**, ротация бэкапов
- Prometheus + Grafana (provisioning дашборда «NAS-Stack», ~23 панели)

### 🔔 Уведомления
- Desktop (`notify-send`, на хосте) и **push в ntfy** на телефон
- События: `torrent.added`, `torrent.completed`, `disk.warning`, `disk.critical`
- Корректная **UTF-8** обработка (rune-safe обрезка) — без «кракозябр» в русских названиях

### ⚙️ UX
- **10 тем** (Catppuccin Mocha/Latte, Dracula, Nord, Tokyo Night, Rosé Pine, Everforest, Kanagawa, Gruvbox, One Dark) + auto
- **Кастомный акцент** (ползунок hue)
- **Командная палитра** (Ctrl+Shift+P), глобальный поиск (Ctrl+K)
- Drag & drop вкладок, экспорт/импорт настроек, скелетоны, хлебные крошки, PWA

### 🩺 Стабильность (v9.1)
- `pprof` на `:6060`, обогащённый `/health` (`mem_mb`, `goroutines`, `gc_count`, `uptime_sec`)
- Prometheus-метрики запросов, лимиты ресурсов, healthcheck, journald-логи
- Memory-leak detector, watchdog с авто-рестартом, смоук-тесты, обновление с авто-откатом

### 🚀 v9.2 (must-have)
- **🏠 «Сегодня»** — вкладка-дашборд: торренты, играющая музыка, диски, YouTube, статус сервисов
- **✨ Умные плейлисты MPD** — «Недавно / Топ месяца / Топ года / Забытые / 🌊 Моя волна» (по истории прослушиваний в PostgreSQL)
- **📊 История прослушиваний** — heatmap 7×24, топы артистов/альбомов/треков, график по дням
- **✨ Умный поиск** — свободная фраза → параметры (жанр/длительность/рейтинг/год) → подбор фильмов
- **🔁 Дубликаты** медиатеки (по `tmdb_id`) + разрешение: файлы переносятся в `/mnt/nas/Backups/duplicates` через host-agent (media смонтирован `ro`)
- **▶️ Продолжить просмотр** — resume-баннер, авто-следующая серия (отсчёт за 10 c), прогресс-бары в списке эпизодов
- **🔐 Редактор `.env`** из UI и **🔄 обновление стека** (run/rollback) через notify-service на хосте
- **📱 PWA-пульт** `?pwa=music` — полноэкранный пульт управления MPD
- **🎬 SponsorBlock** — удаление спонсорских сегментов в YouTube (`--sponsorblock-remove`) + API `/sponsorblock`
- **oom-check** — детектор OOM-kill по kernel-логу

---

## 🎵 Музыка: MPD и браузерный режим

### Linux/macOS
`install.sh` **автоматически**:
1. Устанавливает MPD (`apt` / `pacman` / `dnf` / `zypper` / `brew`).
2. Генерирует `~/.config/mpd/mpd.conf` с путями к вашей музыке.
3. Запускает MPD.
4. NAS-Stack использует **MPD** (Hi-Res, EasyEffects, DSD).

Если MPD уже установлен и настроен — installer не трогает `mpd.conf`.

### Windows
MPD **официально не поддерживается** на Windows (только сборка из исходников).
NAS-Stack автоматически переключается на **браузерный плеер** (HTML5 `<audio>` + транскод в MP3 через ffmpeg).

**Ограничения браузерного режима:**
- Нет Hi-Res (максимум FLAC/MP3/AAC, для DSD/APE/WV — транскод в MP3).
- Нет DSD/нативного вывода.
- Нет фонового воспроизведения (закрыли вкладку — музыка остановилась).

Если нужен Hi-Res на Windows — используйте **WSL2** (Ubuntu + MPD).

### Как это работает
- `music-service` при старте и раз в 2 минуты проверяет MPD (`ping`) → `mode: "mpd" | "browser"`.
- `GET /status` возвращает `mode`; UI сам показывает либо MPD-бар, либо HTML5-плеер.
- `GET /stream/audio/:id` / `GET /browser/stream?file=...` отдают файл с поддержкой Range; для неподдерживаемых браузером форматов — кэшированный MP3-транскод (ffmpeg).
- Библиотека в браузерном режиме строится по файловой системе (`/browser/albums`), MPD не нужен.

## 🏗️ Архитектура

```mermaid
flowchart TB
    UI["web-dashboard :8090<br/>(One-page UI, PWA)"]
    GW["gateway :8080<br/>(JWT, reverse proxy)"]

    UI --> GW

    subgraph core[Микросервисы]
        TOR[torrent :8081]
        STA[stats :8082]
        SCH[scheduler :8084]
        HIS[history :8085]
        AUT[auth :8086]
        BOK[books :8087]
        SEA[search :8088]
        YT[youtube :8089]
        MED[media :8091]
        MUS[music :8092]
    end

    GW --> TOR & STA & SCH & HIS & AUT & BOK & SEA & YT & MED & MUS

    R[("Redis :6379<br/>Pub/Sub + cache")]
    P[("PostgreSQL :5432<br/>nas_history")]

    core -.-> R
    HIS --> P
    MED --> P
    SCH --> P
    MUS --> R

    subgraph obs[Наблюдаемость]
        PROM[prometheus :9090]
        GRAF[grafana :3000]
        PROM --> GRAF
    end
    core -. "/metrics" .-> PROM

    subgraph host[Хост]
        MPD[MPD :6600]
        AGENT[nas-host-agent :9998]
        NOTIFY[notify-service :8083]
        NTFY[ntfy :2586]
    end
    MUS -.-> MPD
    STA -.-> AGENT
    GW -.-> NOTIFY
    NOTIFY -.-> NTFY
```

### Сервисы и порты

| Сервис | Порт | Назначение |
|--------|------|-----------|
| **gateway** | 8080 | Точка входа, реверс-прокси, JWT-авторизация, `/status`, `/health` |
| **torrent-service** | 8081 | Synology DownloadStation: список, добавление, настройки, RuTracker |
| **stats-service** | 8082 | Метрики CPU/RAM/GPU/дисков/сети (`/proc`, `df`, `nvidia-smi`) + host-агент |
| **notify-service** | 8083 | Уведомления (`notify-send` + ntfy). Запускается на **хосте**, вне compose |
| **scheduler-service** | 8084 | Планировщик: бэкапы, сериалы, wishlist, flac-check, healthcheck, disk/memory-check |
| **history-service** | 8085 | История событий и статистика (PostgreSQL + Redis Pub/Sub) |
| **auth-service** | 8086 (internal) | Логин по паролю, выдача JWT. Наружу не публикуется — только через gateway (`/auth/*`) |
| **books-service** | 8087 | Поиск/скачивание книг (Флибуста), отдача EPUB, OPDS |
| **search-service** | 8088 | Поиск торрентов через Prowlarr + Redis-кэш |
| **youtube-service** | 8089 | Скачивание видео (yt-dlp) |
| **web-dashboard** | 8090 | Веб-UI (Go/Fiber, одна HTML-страница, PWA) |
| **media-service** | 8091 | Медиатека: TMDb, транскод, прогресс, коллекции, актёры |
| **music-service** | 8092 | MPD: плеер, метаданные, плейлисты, избранное, статистика |

### Инфраструктура

| Сервис | Порт | Назначение |
|--------|------|-----------|
| redis | 6379 | Кэш и Pub/Sub |
| postgres | 5432 | БД `nas_history` |
| ntfy | 2586 | Push-уведомления на телефон |
| flaresolverr | 8191 | Обход Cloudflare для Prowlarr / RuTracker |
| prowlarr | 9696 | Менеджер индексаторов |
| prometheus | 9090 | Сбор метрик всех сервисов |
| grafana | 3000 | Дашборды (provisioning из `grafana/`) |

### Сопутствующие проекты (вне compose)

- **host-agent** (:9998, папка `host-agent/`) — `lm-sensors`, SMART дисков, размеры дисков,
  очистка. `stats-service` и `web-dashboard` обращаются по `HOST_AGENT_URL`.
- **books-opener** (:9999, папка `books-opener/`) — открытие файлов/VLC на хосте.
  На системах без systemd оба запускаются профилем `nas-non-systemd`.
- **MPD** — на хосте, `0.0.0.0:6600` (доступен контейнерам через `host.containers.internal`).
- **bgutil-pot** (:4416) — вспомогательный сервис для yt-dlp (обход бот-проверок).

---

## 🖥️ Поддерживаемые системы

`scripts/install.sh` автоматически определяет ОС/NAS, runtime и наличие systemd.

| Система | Runtime | Демоны | Статус |
|---------|---------|--------|--------|
| Synology DSM 7.2+ | Docker | Docker-профиль | ✅ |
| QNAP QTS 5.x | Docker | Docker-профиль | ✅ |
| TrueNAS SCALE 24.04+ | Docker | systemd / Docker | ✅ |
| Unraid 6.12+ | Docker | systemd / Docker | ✅ |
| OpenMediaVault 7 | Docker | systemd | ✅ |
| Ubuntu/Debian | Podman/Docker | systemd | ✅ |
| Arch/Fedora | Podman/Docker | systemd | ✅ |
| macOS | Docker Desktop | Docker-профиль | ✅ |
| Windows 10/11 (WSL2) | Docker Desktop | Docker-профиль | ✅ |

Установка: `bash scripts/install.sh` (или `--dry-run`). Демоны без systemd —
профиль `nas-non-systemd` (`notify`, `host-agent`, `books-opener`).

## 🚀 Установка в 1 клик

```bash
curl -fsSL https://raw.githubusercontent.com/FuzzyHD1993/nas-stack/main/scripts/quick-install.sh | bash
```

Скрипт клонирует репозиторий в `~/nas-stack`, запускает `install.sh` и открывает
`http://localhost:8090/setup`.

## 🚀 Быстрый старт

### Первый запуск (wizard)

```bash
git clone git@github.com:FuzzyHD1993/nas-stack.git
cd nas-stack
cp .env.example .env      # .env монтируется в dashboard для wizard
podman compose up -d
```

Откройте **http://localhost:8090/setup** и пройдите 5 шагов:
**Пути → Сеть → API-ключи → Проверка → Готово**. Wizard проверит пути,
сгенерирует `JWT_SECRET` и сохранит `.env` (с резервной копией). Затем:

```bash
podman compose up -d --force-recreate
```

Повторно: `http://localhost:8090/?setup=1`.
Авто-редирект при отсутствии/невалидном `.env`: `SETUP_AUTO_REDIRECT=true`.

### Ручная настройка

```bash
cp .env.example .env
nano .env                        # заполни обязательные поля (см. таблицу)
podman compose up -d --build
# Открыть http://localhost:8090
```

**Обязательные переменные** (остальное имеет разумные дефолты):

| Переменная | Описание | Где взять |
|------------|----------|-----------|
| `NAS_HOST` | IP NAS | `ip a` в вашей сети |
| `NAS_USER` / `NAS_PASS` | DownloadStation | Synology |
| `LIBRARY_ROOT` | Корень медиатеки на NAS | ваш путь |
| `TMDB_API_KEY` | Ключ TMDb | themoviedb.org/settings/api |
| `PROWLARR_API_KEY` | Ключ Prowlarr | Prowlarr → Settings → General |
| `JWT_SECRET` | Секрет JWT | `openssl rand -hex 16` |
| `AUTH_PASSWORD` | Пароль входа | придумайте |
| `HOST_IP` | LAN-IP хоста | `ip a` |

> Все пути/IP/ключи **не захардкожены** — задаются в `.env`. Дефолты в
> `.env.example` рассчитаны на запуск «из коробки» после заполнения секретов.

📖 Подробнее: [установка](docs/installation.md) · [конфигурация](docs/configuration.md) ·
[архитектура](docs/architecture.md) · [API](docs/api.md) · [troubleshooting](docs/troubleshooting.md)

**Требования:**
- **Podman** (или Docker) + compose provider (`docker-compose`, не `podman-compose`);
- **Go 1.27.1** — для локальной сборки/тестов;
- **MPD** на хосте — для музыки;
- **ffmpeg** — внутри `media-service` (уже в образе);
- опционально: **NVIDIA GPU** (nvidia-smi), **`nas-host-agent`** (датчики/SMART), **snapper** (снапшоты).

> ⚠️ **Важно:** после `--force-recreate` отдельного сервиса перезапусти `gateway`, затем
> `web-dashboard`, чтобы контейнеры подхватили новые IP (имена резолвятся через DNS compose).

### Полезные команды

```bash
bash scripts/start-stack.sh                        # надёжный запуск (рекомендуется)
podman compose up -d --build                       # собрать и запустить
podman compose logs -f web-dashboard               # логи сервиса
podman compose up -d music-service web-dashboard   # пересобрать/обновить выбранные
podman compose ps                                  # статус
podman compose down                                 # остановить стек
podman ps --format '{{.Names}}\t{{.Status}}'       # статус контейнеров
bash scripts/smoke-test.sh                          # проверка всего стека
```

---

## 🚀 Запуск стека

### Обычный запуск

`start-stack.sh` поднимает сервисы по порядку (postgres/redis → media → приложения →
gateway → dashboard), ждёт NAS-монтирование, пересоздаёт зависшие контейнеры и
проверяет `/health` на всех портах. Скрипт идемпотентен — можно запускать повторно.

```bash
bash scripts/start-stack.sh
```

### Автозапуск (systemd)

```bash
cp systemd/nas-stack.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable nas-stack.service
systemctl --user start nas-stack.service
```

> Чтобы user-юнит запускался после перезагрузки без входа в сессию, включите linger:
> ```bash
> sudo loginctl enable-linger "$USER"
> ```

### Если что-то упало

```bash
bash scripts/restart-stack.sh
```

---

## 🔑 Переменные окружения (`.env`)

Полный список — в [`.env.example`](.env.example). Ключевые группы:

```bash
# Безопасность / авторизация
JWT_SECRET=change-me
AUTH_PASSWORD=change-me
AUTH_ENABLED=false             # false — JWT не проверяется

# Synology NAS
NAS_HOST=192.168.0.5
NAS_USER=
NAS_PASS=
HOST_IP=192.168.0.4            # LAN-IP хоста, чтобы Prowlarr-ссылки .torrent работали
TORRENT_DESTINATIONS=homes/FuzzyHD/Music,homes/FuzzyHD/Movies,homes/FuzzyHD/TV,...

# Prowlarr / Flibusta
PROWLARR_URL=
PROWLARR_API_KEY=
FLIBUSTA_URL=https://flibusta.is
HTTP_PROXY=

# Медиа (TMDb)
TMDB_API_KEY=
TMDB_LANG=ru-RU
MEDIA_MOVIES_DIR=/movies
MEDIA_TV_DIR=/tv

# Музыка (MPD)
MPD_ADDR=host.containers.internal:6600
BIT_PERFECT_ENABLED=true       # false при использовании EasyEffects/EQ

# YouTube
YOUTUBE_DIR=/youtube
POT_PROVIDER_URL=http://host.containers.internal:4416

# Базы
POSTGRES_URL=postgres://nas:nas_secret@postgres:5432/nas_history?sslmode=disable
REDIS_ADDR=redis:6379

# RuTracker (авто-логин / cookie-fallback)
RUTRACKER_USER=
RUTRACKER_PASS=
RUTRACKER_USER_ID=
RUTRACKER_COOKIE=

# Disk watchdog
DISK_WATCH_INTERVAL_MIN=30
DISK_WARN_PERCENT=80
DISK_CRIT_PERCENT=90

# ntfy (push на телефон, опционально)
NTFY_URL=http://192.168.0.4:2586
NTFY_TOPIC=nas-alerts
NTFY_TOKEN=
```

---

## 🚏 Маршрутизация gateway

`gateway` (:8080) — единая точка входа. Проксирует под-сервисы по префиксам:

| Префикс | → сервис |
|---------|----------|
| `/stats/*` | stats-service :8082 |
| `/torrent/*` | torrent-service :8081 |
| `/notify/*` | notify-service :8083 |
| `/scheduler/*` | scheduler-service :8084 |
| `/history/*` | history-service :8085 |
| `/auth/*` | auth-service :8086 |
| `/books/*` | books-service :8087 |
| `/search/*` | search-service :8088 |
| `/youtube/*` | youtube-service :8089 |
| `/media/*` | media-service :8091 |
| `/music/*` | music-service :8092 |

Служебные: `GET /health`, `GET /status` (карта здоровья всех сервисов), `GET /metrics`.
Веб-UI ходит через `web-dashboard` → `/api/...` → gateway.

---

## ⏰ Планировщик (`scheduler-service`)

Задачи определены в `scheduler-service/main.go`:

| Задача | Интервал | Назначение |
|--------|----------|-----------|
| `healthcheck-services` | 1 мин | Healthcheck всех сервисов, запись в `service_health`, алерты при down/up |
| `memory-check` | 5 мин | Снимок `mem_mb`/`goroutines`/`gc_count` → `memory_checks`, алерты при утечке |
| `oom-check` | 15 мин | Проверка OOM-kill в kernel-логе хоста (через notify-service) |
| `disk-check-detailed` | 6 ч | Заполненность диска через host-agent, двойные пороги + ntfy |
| `wishlist-release-check` | 6 ч | Уведомления о релизах из wishlist |
| `series-check` | 6 ч | Новые серии по подпискам |
| `nas-mount` | 1 ч | Контроль доступности NAS-монтирования |
| `pg-backup` | 24 ч | Дамп PostgreSQL (daily/weekly/monthly + ротация) |
| `backup-configs` | 24 ч | Резервная копия конфигов |
| `flac-check` | 7 дней | Проверка целостности FLAC |

API: `GET /tasks`, `POST /run/:name`, `GET /health/services`, `GET /health/summary`,
`GET /health/history`, `GET /health/memory`, `GET /backup/list`,
`POST /backup/run`, `POST /backup/restore`, `GET /update/status`.

```bash
curl -sS http://localhost:8084/tasks | jq
curl -sS -X POST http://localhost:8084/run/healthcheck-services
curl -sS http://localhost:8084/health/memory | jq '{n:(.services|length), alerts}'
```

---

## 📚 Локальное зеркало Флибусты (опционально)

Если Флибуста недоступна (502), можно поднять локальное зеркало (~200–550 ГБ).
Готового образа нет — образы PHP-FPM/PostgreSQL собираются из исходников:

```bash
git clone --depth 1 https://github.com/zlsl/flibusta.git ../flibusta-mirror
podman compose --profile flibusta-local up -d --build
```

Нужны SQL-дампы (`lib.*.sql.gz`) в `FlibustaSQL/` и zip-архивы книг в
`FLIBUSTA_ARCHIVES_DIR`; после запуска — «Сервис → Обновление базы».

Затем в Wizard включите «Использовать локальную библиотеку Флибусты»
(URL `http://flibusta-local:27100`). `books-service` отдаёт приоритет локальному
зеркалу, затем `flibusta.is` → `.net` → `.lib`. Подробно: [docs/local-flibusta.md](docs/local-flibusta.md).

## 🩺 Стабильность и мониторинг

- **`/health`** каждого сервиса возвращает runtime-метрики:
  `status`, `service`, `version`, `uptime_sec`, `mem_mb`, `mem_sys_mb`, `goroutines`, `gc_count`.
  ```bash
  for p in 8080 8081 8082 8084 8085 8087 8088 8089 8090 8091 8092 8093; do
    curl -sS "http://localhost:$p/health" | jq -c '{service,mem_mb,goroutines}'
  done
  ```
- **pprof** выключен по умолчанию. Включается `PPROF_ENABLED=true` и слушает
  только loopback (`PPROF_ADDR`, по умолчанию `127.0.0.1:6060`) — heap-дампы
  содержат секреты:
  ```bash
  podman exec nas-gateway wget -qO- 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' | head
  podman exec nas-gateway wget -qO- 'http://127.0.0.1:6060/debug/pprof/profile?seconds=10' > cpu.prof
  ```
- **Prometheus-метрики запросов** на `/metrics`: `http_requests_total`,
  `http_request_duration_seconds` (histogram), `http_active_requests`, плюс `go_*` и `process_*`.
  ```bash
  curl -sS http://localhost:8080/metrics | grep '^http_'
  ```
- **Лимиты ресурсов и healthcheck** — в `docker-compose.yml`
  (`deploy.resources.limits`, `healthcheck` через `127.0.0.1`, journald-логирование).
- **Memory leak detector** — задача `memory-check` (таблица `memory_checks`,
  эндпоинт `GET :8084/health/memory`, алерт при +20 goroutines или +30MB памяти за 5 мин).

### Скрипты

| Скрипт | Назначение |
|--------|-----------|
| `scripts/start-stack.sh` | Надёжный запуск: порядок, ожидание NAS, ретраи, health-check всех портов |
| `scripts/watchdog.sh` | Авто-рестарт `unhealthy` контейнеров + notify (systemd-таймер, 5 мин) |
| `scripts/update-stack.sh` | `git pull` → build → up → health-check → **авто-откат** при регрессe |
| `scripts/smoke-test.sh` | Smoke-тест сервисов, host-демонов и инфраструктуры |
| `scripts/btrfs-snapshot.sh` | Снапшот через `snapper` (перед обновлением) |
| `scripts/install-systemd.sh` | Установка user-юнитов в `~/.config/systemd/user/` |
| `scripts/update-indexer-definitions.sh` | Синхронизация Prowlarr-определений с GitHub (v11): sha-skip, бэкап, `podman cp`, рестарт только при изменениях |
| `scripts/add-indexers.sh` | Управление индексерами Prowlarr через REST API: `list / add / test / delete` |
| `scripts/update-rutracker-cookie.sh` | Ручная установка cookie-сессии RuTracker (fallback, если авто-логин не работает) |

```bash
bash scripts/smoke-test.sh        # проверка всего стека (20/20)
bash scripts/update-stack.sh      # обновление с авто-откатом
bash scripts/update-indexer-definitions.sh --status   # только проверка (read-only)
```

> **journald:** ограничение размера журнала задаётся в `systemd/journald-nas.conf`
> (`SystemMaxUse=500M`, `SystemMaxFileSize=50M`, `MaxRetentionSec=1month`).

---

## 🧩 systemd

Стек можно держать как пользовательские юниты (`systemd/`). `nas-notify` работает именно так — вне compose.

```bash
./scripts/install-systemd.sh
systemctl --user enable --now nas-stack.service      # запуск стека через scripts/start-stack.sh
systemctl --user enable --now nas-notify.service     # desktop/push-уведомления
systemctl --user enable --now nas-stack-watchdog.timer  # авто-рестарт нездоровых контейнеров (5 мин)
```

| Юнит | Назначение |
|------|-----------|
| `nas-stack.service` | Запуск стека через `scripts/start-stack.sh` (порядок, ретраи, health-check) |
| `nas-notify.service` | notify-service на хосте |
| `nas-stack-watchdog.service` + `.timer` | Периодический watchdog |
| `nas-prowlarr-definitions.service` + `.timer` | Ежедневное обновление Prowlarr-определений (04:15 ± randomized delay) |
| `journald-nas.conf` | Лимиты журнала journald (не юнит, drop-in) |

> Для ограничения журнала:
> ```bash
> sudo mkdir -p /etc/systemd/journald.conf.d
> sudo cp systemd/journald-nas.conf /etc/systemd/journald.conf.d/nas-stack.conf
> sudo systemctl restart systemd-journald
> ```

---

## 🎧 Мультирум (Snapcast) — опционально

Образ Snapcast не входит в `docker-compose.yml`, есть только `snapserver.conf`.

1. Добавьте сервис snapserver (`ghcr.io/badaix/snapserver`, `network_mode: host` или порты
   1704/1780), смонтировав `./snapserver.conf:/etc/snapserver.conf:ro`.
2. В `~/.config/mpd/mpd.conf` (хост) добавьте FIFO-вывод и перезапустите MPD:
   ```ini
   audio_output {
       type   "fifo"
       name   "Snapcast"
       path   "/tmp/snapfifo"
       format "48000:16:2"
   }
   ```
3. Клиенты подключаются к `HOST_IP:1704` (snapclient).

---

## 🧪 Тесты

### Unit-тесты

`go.work` объединяет 16 модулей (15 сервисов + `internal/platform`):

```bash
make test          # все модули
make lint build    # go vet + go build
# или один сервис:
cd media-service && go test ./... -count=1
```

### Integration-тесты (БД, HTTP)

Нужны PostgreSQL и (опционально) Redis. Тесты помечены `//go:build integration`;
без `TEST_DATABASE_URL` тест БД пропускается.

```bash
export TEST_DATABASE_URL="postgres://nas:nas_secret@localhost:5432/nas_history_test?sslmode=disable"
# ВАЖНО: не указывайте здесь боевую nas_history. Тесты media-service
# создают и удаляют временные схемы (CREATE/DROP SCHEMA) в указанной БД.
for d in media-service music-service books-service gateway; do
    (cd "$d" && go test -tags integration ./... -count=1 -timeout 120s)
done
```

- `media-service` — CRUD в PostgreSQL;
- `music-service` — разбор MPD-метаданных и поиск обложек;
- `books-service` — fallback по зеркалам Flibusta через локальный HTTP-сервер;
- `gateway` — reverse-proxy на тестовый upstream.

В CI работают два job: `test` (все модули) и `integration` (PostgreSQL + Redis).

### Smoke-тесты (E2E)

Требуют запущенный стек, `jq` и настроенные NAS / Prowlarr / Flibusta / MPD:

```bash
bash scripts/smoke-test.sh
```

Проверяются health всех сервисов, host-демоны, инфраструктура и 6 реальных
сценариев: поиск торрентов, медиатека, MPD, поиск книг, читалка FB2/EPUB,
кэш фото.

---

## 🛠 Разработка

Каждый сервис — отдельный Go-модуль (`github.com/FuzzyHD1993/nas-stack/<service>`,
`go 1.27.1`), объединённый `go.work`; сборка через multi-stage Dockerfile
(`golang:1.27-alpine` → `alpine:latest`, контекст — корень репозитория).

```bash
# В каталоге сервиса:
cd music-service
gofmt -w .
go vet ./...
go build ./...
go test ./... -count=1 -timeout 60s

# Локальная сборка образа и рестарт:
cd ..
podman compose build music-service
podman compose up -d music-service

# Общая проверка всех модулей:
make lint build test
```

Общие для всех сервисов файлы:
- `internal/platform` — `GetEnv`, `HealthJSON`, `StartPprof`, `ValidateEnv`;
- `metrics.go` — Prometheus-метрики запросов (`metricsMiddleware()`);
- `main_test.go` — unit-тесты общих хелперов и логики сервиса;
- `go.work` + `Makefile` — сборка/тесты всех 16 модулей одной командой.

### Структура репозитория

```
nas-stack/
├── go.work                     # workspace: 15 сервисов + internal/platform
├── Makefile                    # test / build / lint / fmt
├── docker-compose.yml          # стек (18 сервисов) + лимиты/healthcheck/logging
├── docker-compose.nas.yml      # вариант для NAS
├── prometheus.yml              # scrape-конфиг
├── grafana/provisioning/       # datasource + дашборд «NAS-Stack»
├── snapserver.conf             # пример конфига Snapcast
├── .env.example
├── internal/platform/          # GetEnv, HealthJSON, StartPprof, ValidateEnv
├── auth-service/  stats-service/  torrent-service/  scheduler-service/
├── history-service/  books-service/  search-service/  youtube-service/
├── media-service/  music-service/  gateway/  web-dashboard/
├── notify-service/             # запускается на хосте (systemd)
├── scripts/                    # start-stack, watchdog, smoke-test, ...
├── systemd/                    # user-юниты + journald drop-in
└── web-dashboard/static/       # index.html, app.css, app.js, media/music/..., foliate
```

---

## 🗺 Роадмап

### ✅ Сделано
- **v1.8.0:** ежедневный sync Prowlarr-определений (v11-схема + systemd-таймер), авто-старт
  торрентов при добавлении (`create_authorized` + `taskAction start`), мульти-категории
  в search/torrent-запросах, +6 русских трекеров (Catorrent, PC-torrent, RinTor.NeT,
  sosulki, torrent-pirat, xxxtor).
- **v1.7.0:** локальный SSD-кэш фото (thumbnails/full), обновлённый роадмап.
- **v1.6.0:** читалка FB2/EPUB (foliate-js), кэш `/stats/nas`, фикс тем, `restart-stack.sh`.
- **v1.5.x:** фиксы foliate (`matchMedia`/`paginator`), встроенная читалка.
- **v1.4.x:** локальное зеркало Флибусты (874k книг), прокси-чтение.
- **v1.3.0:** MPD auto-install (Linux/macOS), browser fallback (Windows).
- **v1.0.0:** 1-Click установка, Wizard, мультипрофили, DLNA, Basic Auth.
- **Ранее:** обложки альбомов, 6 «известных грабель», self-healing, ntfy-уведомления.

### 📋 Планы (v1.7+)
- 🔄 **Синхронизация «Продолжить просмотр»** между устройствами.
- 🔍 **Умный поиск** по жанрам, актёрам и году.
- 📊 **Дашборд «Сегодня»**: новые виджеты.
- 🔔 **Telegram-уведомления.**
- 🎮 **Интеграция с играми** (Steam / Lutris).
- 📝 **Авто-теги музыки** (MusicBrainz Picard).
- 🎬 **Трейлеры** (YouTube inline).
- 📱 **Мобильный пульт** (PWA).
- 🖼 **Умные альбомы фото** (YOLO/CLIP).
- 🗺 **Кинокарта** (D3.js).

### 🚀 Дальше
- MPD bit-perfect (для USB-ЦАПов).
- Snapcast (мультирум).
- reMarkable (интеграция).

## 🔧 Troubleshooting

- **`/metrics` отдаёт 500 «collected metric … was collected before».** В Fiber v3
  `c.Path()` / `c.Method()` возвращают zero-copy строки над мутируемым буфером. Их нельзя
  хранить как label-ы Prometheus без копии — используйте `strings.Clone` (уже сделано в
  `metrics.go`).
- **Healthcheck контейнера вечно `(starting)`/`unhealthy`.** `wget` в alpine для
  `localhost` может уйти в IPv6 (`::1`), а сервис слушает IPv4. В healthcheck используйте
  `http://127.0.0.1:<port>/health`.
- **Контейнеры без DNS и интернета, `host.containers.internal` недоступен.**
  Проверьте маршрут в rootless netns:
  ```bash
  podman unshare --rootless-netns ip -4 route show default
  ```
  Пустая строка — сеть стека осталась без IPv4-маршрута (обычно netns создан
  на старте ОС, когда сеть хоста ещё не поднята). Починить:
  ```bash
  scripts/ensure-rootless-net.sh --repair && podman compose up -d
  ```
  `scripts/start-stack.sh` проверяет это сам (пре-флайт), а
  `scripts/watchdog.sh` — каждые 5 минут.
- **`host.containers.internal` не резолвится.** Убедитесь, что у сервиса есть
  `extra_hosts: ["host.containers.internal:host-gateway"]`.
- **После пересоздания сервиса UI отдаёт 502.** `gateway` кэширует IP под-сервисов.
  Перезапустите `gateway`, затем `web-dashboard`:
  ```bash
  podman compose restart gateway
  sleep 2
  podman compose restart web-dashboard
  ```
  `scripts/update.sh` делает это автоматически после `up -d`.
- **Сборка «зависает».** Первая сборка тянет зависимости Go и ffmpeg-пакеты (5–15 мин).
  Не убивайте процесс; следите за прогрессом в отдельном терминале:
  ```bash
  podman compose build <сервис> 2>&1 | tee /tmp/build.log
  tail -f /tmp/build.log
  ```
- **MPD не виден из `music-service`.** Проверьте `MPD_ADDR` и что MPD слушает `0.0.0.0:6600`.
- **Полный перезапуск стека.** Используйте `scripts/restart-stack.sh` (приложения)
  или `scripts/restart-stack.sh --with-db` (вместе с postgres/redis). Скрипт перезапускает
  по порядку, обновляет DNS, проверяет host-порты, лечит зависшие healthcheck и прогревает
  music-index. Не делайте `podman compose up -d --force-recreate` для всех сервисов сразу:
  на rootless Podman возможен сбой conmon (redis/postgres/stats/history) и потеря
  port-forward.
- **UI не открывается с другого устройства.** Дашборд слушает `0.0.0.0:8090` на хосте
  с Podman: используйте `http://<IP-хоста>:8090` (например, `192.168.0.4`), а не IP NAS.
- **Контейнер Up, но health `unhealthy` после рестарта.** Обновите статус:
  `podman healthcheck run <имя>` (или перезапустите контейнер).
- **Не все альбомы показывают обложки.** `music-service` ищет `cover/folder/front/album/artwork/обложка`,
  затем любое изображение в папке (кроме `back/booklet/inlay/...`). Для остальных:
  ```bash
  scripts/fetch-covers.sh --dry-run --limit 20   # посмотреть
  scripts/fetch-covers.sh                        # докачать через iTunes
  ```


---

## 📄 Лицензия

MIT.
