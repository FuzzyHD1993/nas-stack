# REST API (кратко)

Все запросы идут через `gateway` (:8080) по префиксам: `/stats`, `/torrent`,
`/notify`, `/scheduler`, `/history`, `/auth`, `/books`, `/search`, `/youtube`,
(`/api/...`).

## Служебные (каждый сервис)
- `GET /health` — `{status, service, version, uptime_sec, mem_mb, mem_sys_mb, goroutines, gc_count}`
- `GET /metrics` — Prometheus

## gateway
- `GET /status` — карта здоровья всех сервисов
- `POST /search/smart` `{"query":"лёгкая комедия 90 минут"}` — умный поиск фильмов

## music-service
- `GET /status` — текущий трек
- `GET /playlists/smart?type=recent|top_month|top_year|forgotten|wave`
- `GET /listens?limit=` · `DELETE /listens`
- `GET /listens/stats/{daily,hourly,artists,albums,tracks,yearly,heatmap,compare}`
- `POST /saved-playlists/:name/tracks` `{"file": "..."}`
- `GET /audio/chain` — bit-perfect

## media-service
- `GET /movies` · `POST /movies/smart`
- `GET /tv` · `GET /episode/:id/next`
- `GET /duplicates` · `POST /duplicates/resolve`
- `GET/POST /progress/...`

## scheduler-service
- `GET /tasks` · `POST /run/:name`
- `GET /health/services|summary|history|memory`
- `GET /update/status`

## youtube-service
- `GET /jobs` · `POST /download`
- `GET /sponsorblock?video_id=`

## notify-service (хост, :8083)
- `POST /send` `{"text":"...","severity":"info|warning|error"}`
- `GET /env` · `POST /env` · `POST /env/reload`
- `POST /update/run` · `POST /update/rollback`
- `GET /oom-check`

Полный список роутов — в `main.go` соответствующего сервиса.
