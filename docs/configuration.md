# Конфигурация

Все настройки — через `.env` (см. `.env.example`). Значения по умолчанию рассчитаны
на запуск «из коробки» после заполнения секретов. **Ничего не захардкожено** — пути,
IP, ключи и часовой пояс задаются переменными.

## Категории

### Пути
| Переменная | Дефолт | Назначение |
|------------|--------|-----------|
| `NAS_ROOT` | `/mnt/nas` | Корень монтирования NAS |
| `LIBRARY_ROOT` | `/mnt/nas/root/FuzzyHD` | Корень медиатеки |
| `MEDIA_MOVIES_HOST_DIR` | `${LIBRARY_ROOT}/Movies` | Фильмы (хост-путь) |
| `MEDIA_TV_HOST_DIR` | `${LIBRARY_ROOT}/TV` | Сериалы |
| `MEDIA_PHOTOS_HOST_DIR` | `${LIBRARY_ROOT}/Photos` | Фото |
| `MUSIC_HOST_DIR` | `${LIBRARY_ROOT}/Music` | Музыка |
| `BOOKS_HOST_DIR` | `${NAS_ROOT}/Books` | Книги |
| `YOUTUBE_HOST_DIR` | `${NAS_ROOT}/YT_video` | YouTube |
| `BACKUP_HOST_DIR` | `${NAS_ROOT}/Backups` | Бэкапы (в т.ч. дубликаты) |

### Сеть / API
| Переменная | Назначение |
|------------|-----------|
| `HOST_IP` | LAN-IP хоста (для браузера и ссылок) |
| `NAS_HOST` / `NAS_USER` / `NAS_PASS` | Synology DownloadStation |
| `TMDB_API_KEY` / `TMDB_LANG` | TMDb |
| `PROWLARR_URL` / `PROWLARR_API_KEY` | Prowlarr |
| `RUTRACKER_*` | RuTracker (авто-логин/cookie) |
| `FLIBUSTA_URL` | Источник книг |

### Хост-специфика
| Переменная | Дефолт | Назначение |
|------------|--------|-----------|
| `TZ` | `Asia/Novosibirsk` | Часовой пояс контейнеров |
| `BIT_PERFECT_ENABLED` | `false` | Индикатор bit-perfect (выкл. при EQ) |
| `NTFY_TOPIC` | `nas-alerts` | Топик ntfy |
| `BOOKS_OPENER_PUBLIC_URL` | `http://localhost:9999` | Публичный opener для браузера |

### Уведомления (ntfy)
`NTFY_URL`, `NTFY_TOPIC`, `NTFY_TOKEN`.

### Диск-watchdog
`DISK_WATCH_INTERVAL_MIN`, `DISK_WARN_PERCENT`, `DISK_CRIT_PERCENT`.

## Валидация

При старте сервисы проверяют обязательные переменные (`validateEnv`) и падают с
понятной ошибкой, например:

```
❌ отсутствуют обязательные переменные: TMDB_API_KEY — скопируйте .env.example в .env и заполните
```

Обязательные по сервисам:
- **media-service**: `TMDB_API_KEY`, `POSTGRES_URL`;
- **music-service**: `POSTGRES_URL`;
- **torrent-service**: `NAS_HOST`;
- **search-service**: `PROWLARR_API_KEY`;
- **history/scheduler**: `POSTGRES_URL`.
