# Архитектура

## Обзор

`gateway` (:8080) — единая точка входа (JWT + реверс-прокси). `web-dashboard`
(:8090) — одностраничный UI, общается с `gateway`. Сервисы координируются через
**Redis Pub/Sub**, история/статистика — в **PostgreSQL**. Наблюдаемость —
**Prometheus** + **Grafana**.

```
UI :8090 → gateway :8080 → { torrent stats scheduler history auth books search
                 ↕ Redis :6379   ↕ PostgreSQL :5432
Хост: MPD :6600, nas-host-agent :9998, notify-service :8083, ntfy :2586
```

## Сервисы

| Сервис | Порт | Назначение |
|--------|------|-----------|
| gateway | 8080 | Вход, прокси, JWT, `/status` |
| torrent-service | 8081 | Synology DownloadStation, RuTracker |
| stats-service | 8082 | Метрики CPU/RAM/GPU/дисков/сети + host-agent |
| notify-service | 8083 | Уведомления (хост): notify-send + ntfy, update/env/oom |
| scheduler-service | 8084 | Задачи: бэкапы, health, disk/memory/oom-check |
| history-service | 8085 | События и статистика |
| auth-service | 8086 | JWT |
| books-service | 8087 | Флибуста, EPUB, OPDS |
| search-service | 8088 | Prowlarr + кэш |
| youtube-service | 8089 | yt-dlp, SponsorBlock |
| web-dashboard | 8090 | UI (одна страница, PWA) |
| media-service | 8091 | TMDb, транскод, прогресс, дубликаты |
| music-service | 8092 | MPD, метаданные, история прослушиваний, умные плейлисты |

## Наблюдаемость

- `GET /health` — runtime-метрики (`mem_mb`, `goroutines`, `gc_count`, `uptime_sec`).
- `pprof` на `:6060` внутри контейнера.
- `GET /metrics` — `http_requests_total`, `http_request_duration_seconds`,
  `http_active_requests` + `go_*`/`process_*`.

## Общие файлы во всех сервисах

- `stability.go` — pprof-сервер, `healthJSON()`, `validateEnv()`;
- `metrics.go` — Prometheus-middleware (для 12 сервисов);
- `main_test.go` — smoke-тесты.
