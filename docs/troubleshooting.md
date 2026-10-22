# Troubleshooting: топ-10 проблем NAS-Stack

Диагностика начинается с трёх команд:

```bash
podman ps -a --format '{{.Names}}\t{{.Status}}'
podman logs --tail 100 nas-gateway
curl -sS http://127.0.0.1:8090/health
```

Для healthcheck внутри контейнеров используйте `127.0.0.1`, а не `localhost`:
в Alpine `localhost` может резолвиться в IPv6 `::1`, тогда как сервис слушает
IPv4.

## 1. Контейнер не поднимается или постоянно `unhealthy`

```bash
podman ps -a --format '{{.Names}}\t{{.Status}}'
podman logs --tail 200 <service>
podman inspect --format '{{.State.Health.Status}}' <container>
podman inspect --format '{{json .State.Health.Log}}' <container> | jq .
```

Частые причины:
- не заполнены обязательные переменные (`TMDB_API_KEY`, `PROWLARR_API_KEY`,
  `ADMIN_TOKEN`, `POSTGRES_URL`) — сервис намеренно падает fail-closed;
- занят порт: `ss -lntp | grep <port>`;
- нет прав на bind-mount или каталог отсутствует;
- healthcheck стучится в `localhost` вместо `127.0.0.1`.

Не перезапускайте всё подряд: сначала прочитайте последнюю ошибку в логах.

## 2. UI отдаёт 502 или пустой дашборд после пересоздания сервиса

`gateway` и `web-dashboard` кэшируют адреса под-сервисов. После пересоздания
контейнера перезапустите их в правильном порядке:

```bash
podman restart nas-gateway
podman restart nas-dashboard
curl -sS http://127.0.0.1:8080/health
curl -sS http://127.0.0.1:8090/health
```

Если 502 остаётся, проверьте, что сервис действительно слушает порт и что
`host.containers.internal` резолвится внутри контейнера:

```bash
podman exec nas-gateway getent hosts host.containers.internal
podman logs --tail 100 nas-gateway
```

## 3. 401/403: не пускает в UI или админ-ручки

- `401` — невалидный/просроченный JWT. Проверьте `AUTH_ENABLED`,
  `JWT_SECRET`, `AUTH_PASSWORD`; после смены секрета старые токены
  недействительны.
- `403` на `/notify/*`, `/scheduler/*`, `/host-agent/*` — не передан
  `ADMIN_TOKEN` или `NOTIFY_GATEWAY_TOKEN`.
- UI хранит админ-токен в `localStorage` под ключом `admin_token`. Если токен
  сменили, очистите его в DevTools → Application → Local Storage.
- `HISTORY_GATEWAY_TOKEN` пустой — history-service использует fallback
  `NOTIFY_GATEWAY_TOKEN`; если пусты оба, кроме `/health` будет `503`.

Проверка без вывода секрета:

```bash
test -n "$ADMIN_TOKEN" && echo ADMIN_TOKEN_set || echo ADMIN_TOKEN_empty
```

## 4. Не видны фильмы/сериалы/книги, «path not found»

1. Убедитесь, что в `.env` заданы **хостовые** пути
   (`MEDIA_MOVIES_HOST_DIR`, `MEDIA_TV_HOST_DIR`, `BOOKS_HOST_DIR`), а не
   `/movies` и `/tv`.
2. Проверьте mount внутри контейнера:

```bash
podman inspect nas-media --format '{{json .Mounts}}' | jq .
podman exec nas-media ls -la /movies | head
```

3. Проверьте права: контейнеры rootless Podman работают от вашего UID; файлы,
   созданные root/другим UID, могут быть недоступны.
4. Помните, что медиа обычно смонтировано `:ro`; перемещение/удаление делает
   host-agent, а не media-service.

## 5. TMDb не отвечает, нет постеров/метаданных

```bash
curl -sS -o /dev/null -w '%{http_code}\n' \
  "https://api.themoviedb.org/3/configuration?api_key=$TMDB_API_KEY"
```

- `401` — ключ пустой/неверный; проверьте `TMDB_API_KEY`.
- Таймаут/DNS — задайте `TMDB_API_IP` и `TMDB_IMG_IP` или исправьте DNS.
- Пустые постеры при working API — проверьте `TMDB_IMG_IP` и доступ к
  `image.tmdb.org`.
- Не отключайте проверку TLS и не подставляйте чужой ключ.

## 6. Торренты не добавляются / Prowlarr не ищет

```bash
curl -sS http://127.0.0.1:8080/torrent/stats | jq .
podman logs --tail 200 nas-torrent
podman logs --tail 100 nas-search
```

Проверьте:
- `NAS_HOST`, `NAS_USER`, `NAS_PASS` и доступность DownloadStation;
- `PROWLARR_URL`/`PROWLARR_API_KEY` и что Prowlarr видит индексеры;
- `TORRENT_DESTINATION` входит в `TORRENT_DESTINATIONS`;
- при `AUTH_ENABLED=true` — `Authorization: Bearer <JWT>` для `/torrent/*`;
- `FLARESOLVERR_TIMEOUT` при медленных трекерах.

Не добавляйте в `TORRENT_DESTINATIONS` произвольные пути: это allowlist.

## 7. Музыка не играет, MPD недоступен

```bash
podman logs --tail 100 nas-music
ss -lntp | grep 6600
podman exec nas-music getent hosts host.containers.internal
```

- `MPD_ADDR` должен указывать на доступный MPD
  (`host.containers.internal:6600`).
- MPD должен слушать `0.0.0.0:6600`, а не только `127.0.0.1`.
- История прослушиваний пуста — проверьте `POSTGRES_URL`, `nas-postgres`
  и что играются музыкальные расширения.
- `BIT_PERFECT_ENABLED=true` при EasyEffects/EQ может давать неверный
  индикатор; выключите его.

## 8. Книги/Флибуста не открываются

- Проверьте `FLIBUSTA_URL`/`FLIBUSTA_MIRRORS`; зеркала часто меняются.
- Для локального зеркала: `FLIBUSTA_DATA_DIR`, `FLIBUSTA_ARCHIVES_DIR`,
  `FLIBUSTA_DB_PASSWORD`; см. [`local-flibusta.md`](local-flibusta.md).
- `502` от books-service — проверьте `podman logs nas-books` и доступность
  источника.
- Ошибка скачивания `.7z` — проверьте место и целостность архива.
- Не меняйте `FLIBUSTA_DB_PASSWORD` без пересоздания БД зеркала.

## 9. VLC не открывает фильм из дашборда

Симптом в journal: `no suitable interface module`, `interface initialization
failed` — VLC не может подключиться к X/Wayland.

```bash
systemctl --user status nas-books-opener
journalctl --user -u nas-books-opener -n 80 --no-pager
curl -sS http://127.0.0.1:9999/health
```

Проверьте:
- в user-юните `DISPLAY=:0` (Xwayland обычно `:0`, не `:1`);
- `WAYLAND_DISPLAY`, `XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS` заданы;
- `XAUTHORITY` добавляйте только если файл реально существует; в rootless
  Xwayland он часто не нужен;
- путь в `/open-media` абсолютный (`/mnt/nas/root/FuzzyHD/...`);
- `ffprobe` видит H.264/AAC — значит дело не в кодеках.

Ошибки VAAPI (`vaDeriveImage`, `video output creation failed`) могут быть
нефатальными: VLC откатывается на VDPAU/софт. Если воспроизведение идёт,
кодеки не устанавливайте.

## 10. Диск заполнен, OOM или контейнер убит

```bash
df -h /mnt/nas
df -i /mnt/nas
podman system df
journalctl -k -n 200 | grep -iE 'oom|killed process'
journalctl --user -u nas-host-agent -n 100 --no-pager
```

- `dmesg` может быть запрещён (`dmesg_restrict=1`) — используйте
  `journalctl -k`.
- Rootless Podman клампит отрицательные `oom_score_adj` в 0; защищайте
  критичные сервисы через `deploy.resources.limits`.
- Чистите старые бэкапы с учётом `BACKUP_KEEP`, кэш контейнеров — только
  после проверки `podman system df`.
- Не удаляйте файлы библиотеки через `rm`; используйте `trash`/`gio trash`
  и сначала проверяйте, не являются ли они активной раздачей.

## Если ничего не помогло

Соберите диагностику без секретов:

```bash
podman ps -a --format '{{.Names}}\t{{.Status}}'
for c in nas-gateway nas-dashboard; do
  echo "=== $c"; podman logs --tail 80 "$c" 2>&1 | tail -80
done
curl -sS http://127.0.0.1:8090/health
curl -sS http://127.0.0.1:8080/health
```

Приложите вывод к issue и укажите версию (`git describe --tags --always`).
