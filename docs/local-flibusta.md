# Локальное зеркало Флибусты (опционально)

Если Флибуста часто недоступна (502), можно развернуть локальное зеркало
[`zlsl/flibusta`](https://github.com/zlsl/flibusta). Требуется ~200–550 ГБ.

> ⚠️ Готового образа `ghcr.io/zlsl/flibusta` **нет** — образы собираются из
> исходников (PHP-FPM + PostgreSQL + nginx). Профиль `flibusta-local` в
> `docker-compose.yml` уже настроен на локальную сборку.

## 1. Исходники

```bash
cd /home/konstantin/projects
git clone --depth 1 https://github.com/zlsl/flibusta.git flibusta-mirror
```

Путь задаётся в `.env`:

```env
FLIBUSTA_SRC_DIR=/home/konstantin/projects/flibusta-mirror
FLIBUSTA_DATA_DIR=/mnt/nas/root/FuzzyHD/flibusta-data
FLIBUSTA_ARCHIVES_DIR=/mnt/nas/root/FuzzyHD/flibusta-data/Flibusta.Net
FLIBUSTA_DB_PASSWORD=<сгенерированный пароль>
```

## 2. SQL-дампы

Зеркалу нужны **SQL-дампы** (`lib.*.sql.gz`), их нет в торренте с книгами.
Скачивание (HTTPS может отдавать 502 — используйте HTTP):

```bash
cd /mnt/nas/root/FuzzyHD/flibusta-data/FlibustaSQL
for f in lib.libavtor lib.libtranslator lib.libavtorname lib.libbook \
         lib.libfilename lib.libgenre lib.libgenrelist lib.libjoinedbooks \
         lib.librate lib.librecs lib.libseqname lib.libseq lib.reviews \
         lib.b.annotations lib.a.annotations lib.b.annotations_pics lib.a.annotations_pics; do
  curl -fL --retry 3 -o "$f.sql.gz" "http://flibusta.is/sql/$f.sql.gz"
done
for f in *.sql.gz; do gzip -t "$f" || echo "BAD $f"; done
```

Альтернатива — `scripts` из репозитория zlsl: `getsql.sh` (но там `docker`, не `podman`).

## 3. Архивы книг

Если скачан FLibrary-торрент (`fb2.flibusta.lib.rus.ec.7z`), тексты книг лежат
в корне в `*.7z`, а иллюстрации/обложки — в `images/*.zip`. Монтируется весь каталог:

```env
FLIBUSTA_ARCHIVES_DIR=/mnt/nas/root/FuzzyHD/Books/fb2.flibusta.lib.rus.ec.7z
```

Копировать 70 ГБ не нужно — каталог монтируется в контейнер напрямую.

> ⚠️ Оригинальный zlsl умеет читать только `.zip`. Для FLibrary-раскладки в
> контейнер добавлен `p7zip`, а в код — helper `application/archive.php`.
> Готовый патч: [`configs/flibusta-7z.patch`](../configs/flibusta-7z.patch)
> (применять в клоне `flibusta-mirror` через `git apply`).

## 4. Запуск

```bash
podman compose --profile flibusta-local up -d --build
curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:27100/       # 200
curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:27100/opds/  # 200
```

Rootless Podman: контейнер PHP работает с `userns_mode: "keep-id:uid=82,gid=82"`,
поэтому `www-data` (uid 82) внутри контейнера соответствует uid 1000 на хосте и
может писать в `cache/` и `FlibustaSQL/` на CIFS.

## 5. Импорт базы

Через UI: **Сервис → Обновление базы** (`http://localhost:27100`).
Или напрямую (тот же скрипт, что вызывает кнопка):

```bash
podman exec -d nas-flibusta-php sh -c \
  'nohup /application/tools/app_import_sql.sh > /application/sql/import.log 2>&1 &'
podman exec nas-flibusta-php cat /application/sql/status
```

Импорт занимает 1–3 часа (распаковка ~1.7 ГБ SQL + индексы). Прогресс — в
`FlibustaSQL/status` и `FlibustaSQL/import.log`. После завершения:

```bash
podman exec nas-flibusta-db psql -U flibusta -d flibusta -tAc 'SELECT count(*) FROM libbook;'
```

## 6. Подключение к books-service

```env
FLIBUSTA_LOCAL_URL=http://host.containers.internal:27100
```

```bash
podman compose up -d --force-recreate books-service
```

`books-service` отдаёт приоритет локальному зеркалу, затем
`flibusta.is` → `.net` → `.lib`. Если всё недоступно — HTTP 503 с подсказкой.
