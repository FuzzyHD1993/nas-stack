# Troubleshooting

## UI/сервисы

- **После пересоздания сервиса UI отдаёт 502.** Перезапустите `gateway`, затем
  `web-dashboard` — они кэшируют IP под-сервисов.
- **Healthcheck контейнера всегда `(starting)`/`unhealthy`.** В health-check
  используйте `127.0.0.1` (не `localhost`): в alpine `localhost` может резолвиться в
  IPv6 `::1`, а сервис слушает IPv4.
- **`/metrics` возвращает 500 «collected metric … was collected before».** В Fiber v3
  `c.Path()`/`c.Method()` — zero-copy строки над мутируемым буфером; перед
  использованием как label-ов Prometheus нужен `strings.Clone` (уже сделано в
  `metrics.go`).

## Медиа / пути

- **Не находит медиа.** Проверьте `MEDIA_MOVIES_HOST_DIR` / `MEDIA_TV_HOST_DIR` в
  `.env` — это **хост**-пути, монтируемые в `/movies` и `/tv`.
- **TMDb не отвечает.** Проверьте DNS/hosts; при проблемах можно задать фиксированные
  IP через `TMDB_API_IP` / `TMDB_IMG_IP`.

## Музыка

- **MPD не виден из `music-service`.** Проверьте `MPD_ADDR`
  (`host.containers.internal:6600`) и что MPD слушает `0.0.0.0:6600`.
- **`next` уходит в тишину.** Исправлено: при одиночном треке подгружаются соседние
  файлы папки (см. `ensureNextTrack`).
- **История прослушиваний пуста.** Нужны `POSTGRES_URL` и запущенный `postgres`;
  записи добавляются `playTracker` (только музыкальные расширения).

## Хост-агент

- **`host.containers.internal` не резолвится.** У сервиса должен быть
  `extra_hosts: ["host.containers.internal:host-gateway"]`.
- **SMART/permissions.** `smartctl` требует root — агент пробует `sudo -n`.
- **Дубликаты не перемещаются.** Медиа смонтировано `:ro`; перемещение выполняет
  `nas-host-agent` в `/mnt/nas/Backups/duplicates`.

## OOM

- **`dmesg` запрещён (`dmesg_restrict=1`).** OOM-события читаются через
  `journalctl -k` (см. `notify-service /oom-check`).
- **`oom_score_adj` не применяется.** Rootless podman клампит отрицательные значения
  в 0 — защита критичных сервисов идёт через `deploy.resources.limits`.
