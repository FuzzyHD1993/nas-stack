# NAS-Stack: навыки и безопасное скачивание

Практическая инструкция: как пользоваться OpenCode-навыками `nas-admin` и
`sysadmin`, как устроен прокси скачивания `search-service` и как обслуживать
стек после security-фикса.

Актуально на commit `dac3280` (`fix(security): remove Prowlarr API key from
search results`).

---

## 1. Что где лежит

| Компонент | Где | Порт |
|---|---|---|
| `search-service` | контейнер `nas-search`, каталог `search-service/` | `127.0.0.1:8088` |
| gateway | контейнер `nas-gateway`, каталог `gateway/` | `0.0.0.0:8080` |
| web-dashboard | контейнер `nas-dashboard`, каталог `web-dashboard/` | через gateway |
| torrent-service | контейнер `nas-torrent` | `127.0.0.1:8081` |
| Redis | контейнер `nas-redis` | `redis:6379` (внутри сети) |
| Prowlarr | контейнер, доступен как `host.containers.internal:9696` | `9696` |
| Навыки | `~/.config/opencode/skills/` | — |
| Репозиторий навыков | `~/projects/opencode-skills` (private) | — |

В `.env` задан `HOST_IP=192.168.0.4` (LAN-адрес хоста CachyOS); в
`docker-compose.yml` значение по умолчанию пустое (`HOST_IP=${HOST_IP:-}`),
поэтому переменная обязательна — без неё torrent-service не перепишет
`host.containers.internal`. NAS/DSM — `192.168.0.5`.

---

## 2. Навыки OpenCode

### 2.1. `nas-admin`

Файл: `~/.config/opencode/skills/nas-admin.md` (ID `nas-admin`).

Для чего: NAS-Stack, торренты, Fish-функции, медиа-сервисы, конфиги.

Примеры запросов:

- «Добавь торрент `magnet:?xt=...` в Movies»
- «Проверь статистику торрентов»
- «Почему `nas-torrent` не качает?»
- «Создай Fish-функцию для очистки кэша»
- «Найди фильм в search-service и добавь в торренты»
- «Поправь `.env` через notify-service»

Правила: удаление только через `trash-cli`; конфиги — с бэкапом и diff;
`sudo` — без явной просьбы не выполнять.

### 2.2. `sysadmin`

Файл: `~/.config/opencode/skills/sysadmin/SKILL.md` (ID `sysadmin`).

Для чего: CachyOS/Linux — диски, пакеты, systemd, сеть, логи, Btrfs,
безопасность.

Примеры запросов:

| Запрос | Действие |
|---|---|
| «Проверь место на диске» | `df -h /`, `df -h /mnt/nas`, `df -i /` |
| «Почему упал `<сервис>`?» | `systemctl status`, `journalctl -u` |
| «Почисти кэш пакетов» | подтверждение → `pacman -Sc` / `paccache -r` |
| «Удали снапшот 42» | подтверждение → `snapper delete 42` |
| «Покажи ошибки за загрузку» | `journalctl -p err -b` |
| «Проверь порты» | `ss -tlnp` |

Правила после аудита:

- диагностика (`df`, `du`, `status`, `journalctl`, `snapper list`) — свободно;
- изменения (`pacman -Syu`, `-Rns`, очистка кэша, `restart`, `snapper delete`) —
  **только после явного подтверждения**;
- `rm` запрещён; используй `trash` или `gio trash` (в bash `fs rm` недоступен);
- `/etc/fstab`, `/boot`, загрузчик — только с бэкапом;
- после изменения — проверка (`systemctl is-active`, `pacman -Q`, `df -h /`,
  `snapper list`).

### 2.3. Как вызывать

OpenCode подгружает навык автоматически, когда запрос совпадает с
`description`. Можно явно:

> «Используй навык sysadmin и проверь место на диске»

Навык не является отдельной командой — это инструкция для агента. Чем точнее
запрос, тем вероятнее нужный навык.

### 2.4. Как добавить или изменить навык

1. Создай `~/.config/opencode/skills/<id>/SKILL.md` (или `<id>.md`).
2. Frontmatter:

   ```yaml
   ---
   name: my-skill
   description: Когда и для чего использовать навык.
   ---
   ```

3. Перезапусти OpenCode/сессию. Проверь, что навык появился в списке
   доступных.

Не держи одновременно flat-файл и каталог с тем же ID в одном источнике.

### 2.5. Репозиторий навыков

`~/projects/opencode-skills` → GitHub private. Структура:

```
skills/
├── nas-admin/SKILL.md
└── sysadmin/SKILL.md
```

Установка из репозитория:

```bash
cp -r ~/projects/opencode-skills/skills/sysadmin ~/.config/opencode/skills/
```

Обновление и релиз:

```bash
cd ~/projects/opencode-skills
git add -A
git commit -m "feat(sysadmin): ..."
git push github main
git tag v0.2.0
git push github v0.2.0
```

---

## 3. `search-service`: `downloadUrl` через прокси

### 3.1. Что изменилось

Раньше `/search` отдавал ссылку Prowlarr вместе с `apikey`:

```json
"downloadUrl": "http://host.containers.internal:9696/2/download?apikey=<KEY>&link=..."
```

Теперь ключ и хост Prowlarr клиенту не видны:

```json
"downloadUrl": "http://host.containers.internal:8080/search/download?url=%2F2%2Fdownload%3F...%26link%3D..."
```

Сервер сам добавляет `X-Api-Key` и `apikey` при запросе к Prowlarr.

Дополнительно:

- кэш поиска переведён на `v3` — старые записи с ключом не читаются;
- gateway больше не режет `Location: magnet:` (Prowlarr отвечает `302` на
  magnet для части индексаторов);
- таймаут gateway для `/search/*` — 60 с (как у search-service).

### 3.2. Как пользоваться

**В дашборде** — ничего делать не нужно. Кнопка «⬇️ Скачать» отправляет
`magnet` или `downloadUrl` в `/api/torrents`; torrent-service переписывает
`host.containers.internal` на `HOST_IP` и передаёт URL в DSM. Если Prowlarr
отвечает `302` на magnet, torrent-service сам разрешает редирект
(`resolveMagnet`) и добавляет magnet в DSM; если отдаёт `.torrent`, DSM
получает URL с переписанным хостом.

**Вручную (curl):**

```bash
# 1. Получить первый результат
curl -s "http://127.0.0.1:8088/search?q=matrix&limit=1" | jq '.results[0]'

# 2. Проверить, что ключа нет (0)
curl -s "http://127.0.0.1:8088/search?q=matrix" | grep -c apikey

# 3. Скачать через прокси (с хоста; на другой машине замени 127.0.0.1 на 192.168.0.4)
u=$(curl -s "http://127.0.0.1:8088/search?q=matrix&limit=1" | jq -r '.results[0].downloadUrl')
curl -s -D- -o /tmp/result.bin "http://127.0.0.1:8080${u#http://host.containers.internal:8080}"

# 4. Если 200 — это .torrent:
file /tmp/result.bin        # BitTorrent file

# 5. Если 302 — Prowlarr отдал magnet (файла нет, это нормально):
#    в выводе заголовков будет Location: magnet:?xt=urn:btih:...
```

`host.containers.internal` — имя, которое резолвится только внутри
контейнеров. На хосте используй `127.0.0.1`, из LAN — `192.168.0.4`.

### 3.3. Контракт эндпоинта

```
GET /search/download?url=<url-encoded path+query>
```

- только пути `/{id}/download` или `/api/v1/indexer/{id}/download`;
- обязателен непустой `link`;
- хост из запроса игнорируется — всегда `prowlarrURL` (SSRF невозможен);
- `apikey` клиента вырезается, серверный добавляется в `X-Api-Key` и `apikey`;
- ответ: `.torrent` (`application/x-bittorrent`, до 16 MB) или `302` на
  `magnet:`/внешний URL;
- ошибки: `400 invalid download url`, `502 upstream unavailable`,
  `502 download too large`.

Внутри search-service тот же обработчик доступен как `GET /download` — по
этому пути его вызывает gateway (`/search/*` → `/download`).

### 3.4. Ошибки

| Ответ | Причина |
|---|---|
| `400 invalid download url` | Путь не `/{id}/download`, нет `link`, передан чужой хост или полный URL. |
| `502 upstream unavailable` | Prowlarr недоступен/медленный; смотри `podman logs nas-search`. |
| `502 download too large` | Ответ Prowlarr больше 16 MB. |
| `401`/`403` | `AUTH_ENABLED=true`; клиент gateway (дашборд/curl) не передаёт JWT и получает 401. DSM к gateway напрямую не обращается. Нужен exemption для `/search/download` или service-token. Сейчас auth выключен. |
| Пустой `downloadUrl` | У результата нет magnet и ссылка Prowlarr не похожа на путь скачивания — результат отбрасывается. |

### 3.5. Кэш

Ключи: `search:torrents:v3:*`. Старые `v2` не читаются. Они доживают до TTL
(~1 час); при желании удалить:

```bash
podman exec nas-redis redis-cli --scan --pattern 'search:torrents:v2:*' \
  | while read -r key; do podman exec nas-redis redis-cli DEL "$key" >/dev/null; done
```

---

## 4. Обслуживание

Пересборка после изменений:

```bash
cd ~/projects/nas-stack
podman compose build search-service gateway web-dashboard
podman compose up -d --force-recreate search-service gateway web-dashboard
```

Логи:

```bash
podman logs --tail 100 nas-search
podman logs --tail 100 nas-gateway
podman logs --tail 100 nas-torrent
```

Health:

```bash
curl -s http://127.0.0.1:8088/health | jq
curl -s http://127.0.0.1:8080/health | jq
```

Откат security-фикса:

```bash
git revert dac3280
podman compose build search-service gateway
podman compose up -d --force-recreate search-service gateway
```

---

## 5. Troubleshooting

| Симптом | Решение |
|---|---|
| В UI скачивание не работает, `curl` с хоста работает | `host.containers.internal` не резолвится в браузере/DSM; проверь `HOST_IP` и доступность `192.168.0.4:8080`. |
| `502` на скачивании | Проверь Prowlarr (`:9696`) и `podman logs nas-search`. |
| `401` после включения auth | Освободить `/search/download` (capability URL) или добавить service-token. |
| Поиск отдаёт старые результаты | Проверь префикс `v3` в ключах Redis; старые `v2` не читаются. |
| `downloadUrl` пустой | Нет magnet и путь Prowlarr не прошёл allowlist — результат отброшен. |

---

## 6. Безопасность и остаточные риски

- **Ключ Prowlarr** не попадает в `/search`, `downloadUrl`, `Location` и логи;
  в ошибках он маскируется.
- **SSRF**: прокси ходит только на `prowlarrURL`; путь ограничен
  `/{id}/download`; `link` — обычный query-параметр.
- **Остаточный риск 1**: при `AUTH_ENABLED=true` `/search/*` и `/torrent/*`
  через gateway закрыты JWT; клиент (дашборд/curl) не передаёт токен и получает
  401. DSM к gateway напрямую не обращается. Нужен exemption/service-token.
- **Остаточный риск 2**: Prowlarr может отдать `302` на произвольный http(s)
  URL (open redirect внутри доверия к Prowlarr). Ограничивать нельзя без
  поломки легитимных редиректов индексаторов.
- **Остаточный риск 3**: очень длинные `link` после кодирования могут превысить
  лимит torrent-service (4096). На практике ссылки ~1 KB.
- **Остаточный риск 4**: старые ключи `v2` физически лежат в Redis до TTL, но
  не отдаются.

---

## 7. История

| Дата | Что |
|---|---|
| 2026-10-21 | `dac3280`: убрана утечка `apikey` из `/search`; добавлен прокси скачивания; gateway пропускает `Location: magnet`; кэш `v3`. |
| 2026-10-21 | `sysadmin` v0.2.0: аудит двумя ревьюерами, исправлены 7 дефектов; добавлен в `opencode-skills`. |
