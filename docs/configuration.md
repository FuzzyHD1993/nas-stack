# Конфигурация NAS-Stack

Все настройки стека задаются через `.env` в корне репозитория. Шаблон —
[`.env.example`](../.env.example). `.env` содержит секреты и **не должен**
попадать в git.

```bash
cp .env.example .env
# заполните обязательные значения (в таблицах отмечены «да»)
```

Compose подставляет значения `${VAR}` при разборе файла. Переменные из `.env`
не export-ятся в текущую shell-сессию; для ручных команд используйте
`set -a; . ./.env; set +a` только если понимаете последствия.

## Пути к хранилищу

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `NAS_ROOT` | `/mnt/nas` | да | Корень смонтированного NAS или локальной папки с данными |
| `LIBRARY_ROOT` | `/mnt/nas/root/FuzzyHD` | да | Корень медиатеки: фильмы, сериалы, музыка, фото |
| `BACKUP_HOST_DIR` | `${NAS_ROOT}/Backups` | нет | Бэкапы БД и карантин дубликатов |
| `BOOKS_HOST_DIR` | `${NAS_ROOT}/Books` | нет | Книги на хосте |
| `YOUTUBE_HOST_DIR` | `${NAS_ROOT}/YT_video` | нет | Скачанные YouTube-видео |
| `MEDIA_MOVIES_HOST_DIR` | `${LIBRARY_ROOT}/Movies` | нет | Фильмы (хост-путь, не путь внутри контейнера) |
| `MEDIA_TV_HOST_DIR` | `${LIBRARY_ROOT}/TV` | нет | Сериалы |
| `MEDIA_PHOTOS_HOST_DIR` | `${LIBRARY_ROOT}/Photos` | нет | Фото |
| `MUSIC_HOST_DIR` | `${LIBRARY_ROOT}/Music` | нет | Музыка; здесь же обычно `New_Stuff` |

Пути в `.env` — **хостовые**. В контейнеры они монтируются как `/movies`,
`/tv`, `/music`, `/books` и т.д. Если меняете `LIBRARY_ROOT`, проверьте
соответствующие volume в `docker-compose.yml`/`podman-compose`.

## Сеть и базы

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `HOST_IP` | `192.168.0.4` | да | LAN-IP хоста для ссылок и браузера |
| `NAS_HOST` | `192.168.0.5` | да | Адрес Synology/QNAP для DownloadStation |
| `NAS_USER` | — | нет | Пользователь NAS |
| `NAS_PASS` | — | нет | Пароль NAS (секрет) |
| `NAS_STRICT_HOST_KEY` | `accept-new` | нет | SSH host-key policy: `accept-new` (TOFU) или `yes` |
| `NAS_KNOWN_HOSTS` | `/root/.ssh/known_hosts` | нет | Путь к known_hosts в контейнере |
| `REDIS_ADDR` | `redis:6379` | нет | Redis внутри compose-сети |
| `POSTGRES_URL` | `postgres://nas:nas_secret@postgres:5432/nas_history?sslmode=disable` | да | DSN PostgreSQL |
| `POSTGRES_USER` | `nas` | нет | Пользователь PostgreSQL |
| `POSTGRES_PASSWORD` | `nas_secret` | да | Пароль PostgreSQL; смените для внешнего доступа |
| `POSTGRES_DB` | `nas_history` | нет | Имя БД |

`NAS_STRICT_HOST_KEY=yes` используйте только после того, как known_hosts
заранее заполнен в volume `scheduler_ssh`.

## Авторизация и токены

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `JWT_SECRET` | пусто | при `AUTH_ENABLED=true` | Подпись JWT; `openssl rand -hex 32` |
| `AUTH_PASSWORD` | пусто | при `AUTH_ENABLED=true` | Пароль входа, минимум 8 символов, не `admin` |
| `AUTH_ENABLED` | `false` | нет | Включает `/login` и проверку JWT |
| `AUTH_TOKEN_TTL` | `168h` | нет | Время жизни JWT (7 дней) |
| `PPROF_ENABLED` | `false` | нет | pprof только на loopback (`PPROF_ADDR`) |
| `NOTIFY_GATEWAY_TOKEN` | пусто | для host-exec | Секрет gateway → notify-service (`X-Gateway-Token`) |
| `HISTORY_GATEWAY_TOKEN` | пусто | нет | Секрет gateway → history-service; пусто = fallback на `NOTIFY_GATEWAY_TOKEN` |
| `ADMIN_TOKEN` | пусто | для host-exec | Bearer-токен админ-операций; gateway/scheduler не стартуют без него |
| `HOST_AGENT_BIND` | `127.0.0.1:9998,192.168.0.4:9998` | нет | Адреса host-agent через запятую |

При `AUTH_ENABLED=true` пустой/короткий/дефолтный `JWT_SECRET` или пустой
`AUTH_PASSWORD` приводят к отказу старта. `ADMIN_TOKEN` защищает
`/notify/update/*`, `/notify/env*`, `/scheduler/update/run`, host-agent
`/update/run`, `/cleanup/run`, `/duplicates/resolve`. UI запрашивает его один
раз и хранит в `localStorage` под ключом `admin_token`.

`HOST_AGENT_BIND` должен содержать LAN-IP хоста, если контейнеры ходят на
`host.containers.internal:9998`: в rootless Podman этот адрес не loopback.

## TMDb и медиатека

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `TMDB_API_KEY` | пусто | да для media-service | API-ключ TMDb |
| `TMDB_LANG` | `ru-RU` | нет | Язык метаданных |
| `TMDB_API_IP` | `143.204.238.23` | нет | Фиксированный IP TMDb API для обхода DNS |
| `TMDB_IMG_IP` | `185.59.220.199` | нет | Фиксированный IP image.tmdb.org |

Ключ: <https://www.themoviedb.org/settings/api>. При проблемах с DNS
используйте `TMDB_API_IP`/`TMDB_IMG_IP` или локальный DNS; не отключайте
проверку TLS.

## Prowlarr и торренты

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `PROWLARR_URL` | пусто | да для поиска | Базовый URL Prowlarr |
| `PROWLARR_API_KEY` | пусто | да для search-service | API-ключ Prowlarr |
| `DOWNLOAD_BASE_URL` | `http://host.containers.internal:8080` | нет | Прокси скачивания `.torrent` |
| `FLARESOLVERR_TIMEOUT` | `120000` | нет | Таймаут FlareSolverr, мс |
| `TORRENT_DESTINATION` | `homes/FuzzyHD/Music` | нет | Каталог по умолчанию для magnet |
| `TORRENT_DESTINATIONS` | `homes/FuzzyHD/Music,homes/FuzzyHD/Movies,...` | нет | Разрешённые каталоги через запятую |
| `TORRENT_DESTINATION_MUSIC` | — | нет | Переопределение категории Music (закомментировано в шаблоне) |
| `TORRENT_DESTINATION_MOVIES` | — | нет | Переопределение категории Movies |
| `RUTRACKER_USER` | пусто | нет | Логин RuTracker |
| `RUTRACKER_PASS` | пусто | нет | Пароль RuTracker (секрет) |
| `RUTRACKER_USER_ID` | пусто | нет | ID пользователя RuTracker |
| `RUTRACKER_COOKIE` | пусто | нет | Cookie-fallback (секрет) |

`TORRENT_DESTINATIONS` — относительные пути внутри DownloadStation, не
хостовые. Добавляйте только те каталоги, куда действительно разрешено
складывать торренты.

## Музыка, MPD и OOM

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `MPD_ADDR` | `host.containers.internal:6600` | да для music-service | Адрес MPD |
| `BIT_PERFECT_ENABLED` | `false` | нет | Индикатор bit-perfect; выключите при EasyEffects/EQ |
| `OOM_CHECK_MINUTES` | `20` | нет | Глубина просмотра kernel-лога для OOM |

MPD должен слушать интерфейс, доступный контейнеру (обычно `0.0.0.0:6600`).

## Книги и Флибуста

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `FLIBUSTA_URL` | `https://flibusta.is` | нет | Основной источник |
| `FLIBUSTA_MIRRORS` | `https://flibusta.is,https://flibusta.net,https://flibusta.lib` | нет | Список зеркал через запятую |
| `FLIBUSTA_LOCAL_URL` | пусто | нет | URL локального зеркала |
| `FLIBUSTA_DATA_DIR` | `/mnt/nas/root/FuzzyHD/flibusta-data` | нет | Данные локального зеркала |
| `FLIBUSTA_ARCHIVES_DIR` | `/mnt/nas/root/FuzzyHD/Books/fb2.flibusta.lib.rus.ec.7z` | нет | Архив FB2 для локального зеркала |
| `FLIBUSTA_SRC_DIR` | `../flibusta-mirror` | нет | Исходники/скрипты зеркала |
| `FLIBUSTA_DB_PASSWORD` | `change-me` | да для локального зеркала | Пароль БД; смените |
| `HTTP_PROXY` | пусто | нет | Прокси для внешних запросов |

Для локального зеркала см. [`local-flibusta.md`](local-flibusta.md).

## YouTube

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `POT_PROVIDER_URL` | `http://host.containers.internal:4416` | нет | bgutil-pot provider на хосте |

## Медиа: watcher и транскод

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `MEDIA_WATCH_ENABLED` | `true` | нет | Следить за новыми файлами |
| `MEDIA_WATCH_DEBOUNCE_SEC` | `3` | нет | Задержка перед обработкой, сек |
| `MEDIA_WATCH_POLL_SEC` | `20` | нет | Период опроса, сек |
| `TRANSCODE_VIDEO_ENABLED` | `true` | нет | Включить транскод |
| `TRANSCODE_ENCODER` | `auto` | нет | `auto`, `vaapi`, `nvenc`, `libx264` и т.п. |
| `TRANSCODE_PRESET` | `veryfast` | нет | Пресет энкодера |
| `TRANSCODE_CRF` | `23` | нет | Качество (меньше — лучше/больше) |

## Host-agent, books-opener и диск

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `HOST_AGENT_URL` | `http://host.containers.internal:9998` | нет | Адрес host-agent из контейнеров |
| `BOOKS_OPENER_PUBLIC_URL` | `http://localhost:9999` | нет | Публичный URL opener для браузера |
| `OPENER_TOKEN` | пусто | для service-to-service | Токен opener; браузер его не знает |
| `OPENER_ORIGINS` | `http://localhost:8090,http://127.0.0.1:8090` | нет | CORS allowlist для opener |
| `DISK_WATCH_INTERVAL_MIN` | `30` | нет | Период disk-watchdog, мин |
| `DISK_WARN_PERCENT` | `80` | нет | Порог warning, % |
| `DISK_CRIT_PERCENT` | `90` | нет | Порог critical, % |

`OPENER_TOKEN` должен совпадать у media-service и books-opener, если включён
service-to-service доступ. `OPENER_ORIGINS` не используйте со `*` при
публикации opener в LAN.

## Уведомления и прочее

| Переменная | По умолчанию | Обяз. | Назначение |
|---|---|---:|---|
| `NTFY_URL` | `http://192.168.0.4:2586` | нет | Сервер ntfy |
| `NTFY_TOPIC` | `nas-alerts` | нет | Топик уведомлений |
| `NTFY_TOKEN` | пусто | нет | Bearer-токен ntfy (секрет) |
| `TZ` | `Asia/Novosibirsk` | нет | Часовой пояс контейнеров |
| `BACKUP_KEEP` | `7` | нет | Сколько бэкапов БД хранить |

## Валидация при старте

Сервисы проверяют обязательные переменные и падают с понятной ошибкой:

```text
❌ отсутствуют обязательные переменные: TMDB_API_KEY — скопируйте .env.example в .env и заполните
```

Минимум по сервисам:

| Сервис | Обязательные переменные |
|---|---|
| `media-service` | `TMDB_API_KEY`, `POSTGRES_URL` |
| `music-service` | `POSTGRES_URL` |
| `torrent-service` | `NAS_HOST` |
| `search-service` | `PROWLARR_API_KEY` |
| `history-service` / `scheduler-service` | `POSTGRES_URL` |
| `gateway` / `scheduler-service` | `ADMIN_TOKEN` (fail-closed host-exec) |

## Безопасная правка `.env`

1. Сделайте бэкап: `cp .env .env.bak-$(date +%Y%m%d-%H%M%S)`.
2. Меняйте только нужные ключи; не перезаписывайте файл целиком.
3. Для штатной правки через стек используйте `POST /notify/env` с
   `Authorization: Bearer <NOTIFY_GATEWAY_TOKEN>` (защищённые ключи вроде
   `PATH`, `DOCKER_HOST` не записываются).
4. После изменения перезапустите затронутые сервисы и проверьте `/health`.
5. Не коммитьте `.env`, `*.bak` и токены.
