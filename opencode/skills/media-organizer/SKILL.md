---
name: Media Organizer
description: Безопасная сортировка New_Stuff в Movies, TV и Music по TMDb/MusicBrainz с dry-run, проверкой сидов и отчётом.
---

# Media Organizer

Ты — библиотекарь медиатеки NAS-Stack. Задача: разобрать файлы из `New_Stuff`
по каталогам `Movies`, `TV`, `Music`, не сломав раздачи, не потеряв файлы и
не изменив `.env` или секреты.

## Главные правила

1. **Сначала dry-run.** По умолчанию только показывай план: источник → цель,
   тип, уверенность совпадения, причина. Перемещение — только после явного
   `--apply` от пользователя.
2. **Не трогай активные раздачи.** Если файл или каталог находится в
   `New_Stuff` и на него ссылается активный торрент, пропусти его и запиши
   в отчёт. Перемещение сломает сидирование.
3. **Не удаляй без подтверждения.** Для мусора используй `trash-put`/`gio trash`,
   а не `rm`. Дубликаты не удаляй автоматически.
4. **Не читай и не печатай секреты.** `TMDB_API_KEY` бери из окружения или
   `.env` только на чтение; в отчёт не выводи значение.
5. **Не переименовывай существующую библиотеку.** Новые файлы раскладывай по
   предложенной схеме; массовую реорганизацию старых папок делай только
   отдельной задачей с подтверждением.
6. **Сохраняй метаданные.** Для музыки переноси весь релиз: аудио, `cover.jpg`,
   `.cue`, `.log`, `.m3u8`; не разрывай альбом на отдельные треки.
7. **Каждый перенос фиксируй** в manifest: `sha256`, исходный путь, целевой
   путь, время, результат. Manifest храни в `/tmp` или рядом с `New_Stuff`,
   не в медиатеке.

## Пути

Значения по умолчанию (хост-пути, не пути внутри контейнеров):

| Переменная | Значение |
|---|---|
| `LIBRARY_ROOT` | `/mnt/nas/root/FuzzyHD` |
| `MEDIA_MOVIES_HOST_DIR` | `/mnt/nas/root/FuzzyHD/Movies` |
| `MEDIA_TV_HOST_DIR` | `/mnt/nas/root/FuzzyHD/TV` |
| `MUSIC_HOST_DIR` | `/mnt/nas/root/FuzzyHD/Music` |
| `NEW_STUFF_DIR` | `${MUSIC_HOST_DIR}/New_Stuff` |

Фактический источник может быть передан аргументом. Если `New_Stuff` нет,
проверь `${LIBRARY_ROOT}/New_Stuff` и `MUSIC_HOST_DIR/New_Stuff`; не создавай
каталог без необходимости.

## 1. Инвентаризация

```bash
SRC="${NEW_STUFF_DIR:-/mnt/nas/root/FuzzyHD/Music/New_Stuff}"
find "$SRC" -mindepth 1 -maxdepth 2 -type f \
  \( -iname '*.mkv' -o -iname '*.mp4' -o -iname '*.avi' -o -iname '*.mov' \
     -o -iname '*.webm' -o -iname '*.m4v' -o -iname '*.ts' \
     -o -iname '*.flac' -o -iname '*.mp3' -o -iname '*.m4a' \
     -o -iname '*.ogg' -o -iname '*.opus' -o -iname '*.wav' \) \
  -printf '%s\t%p\n' | sort -n
```

Для видео сними реальные параметры, а не только имя:

```bash
ffprobe -v error -show_entries \
  format=duration:format_tags=title,artist,album,date,show,season,episode \
  -of json "$file"
```

## 2. Определение типа

- **TV**: в имени есть `SxxExx`, `Season N`, `NxNN`, или `ffprobe` показывает
  `show`/`season`/`episode`.
- **Movie**: видеофайл без признаков сезона/эпизода.
- **Music**: аудиофайлы, а также каталог с `cover.jpg`, `.cue`, `.log`, `.m3u8`.
- Если тип неочевиден — не угадывай, положи в `_Review` и объясни причину.

## 3. Метаданные

### Movies / TV — TMDb

Ключ: `TMDB_API_KEY` из окружения. Язык: `TMDB_LANG`, по умолчанию `ru-RU`.

```bash
# Фильм
curl -fsS --get 'https://api.themoviedb.org/3/search/movie' \
  --data-urlencode "api_key=$TMDB_API_KEY" \
  --data-urlencode "query=$title" \
  --data-urlencode "year=$year" \
  --data-urlencode "language=${TMDB_LANG:-ru-RU}"

# Сериал
curl -fsS --get 'https://api.themoviedb.org/3/search/tv' \
  --data-urlencode "api_key=$TMDB_API_KEY" \
  --data-urlencode "query=$title" \
  --data-urlencode "first_air_date_year=$year" \
  --data-urlencode "language=${TMDB_LANG:-ru-RU}"
```

Правила выбора:
- Сравнивай нормализованные `title`/`original_title`, год и тип.
- Для TV дополнительно проверь сезон/эпизод через `/3/tv/{id}/season/{n}`.
- Если несколько кандидатов или уверенность низкая — `_Review`, не перемещай.
- При `429` сделай паузу и повтори; при `401` не подставляй другой ключ, а
  сообщи, что `TMDB_API_KEY` не задан/неверен.

### Music — MusicBrainz

TMDb не индексирует музыку, поэтому для исполнителя/альбома используй
MusicBrainz (ключ не нужен, обязателен осмысленный `User-Agent`):

```bash
curl -fsS --get 'https://musicbrainz.org/ws/2/release-group' \
  --data-urlencode "query=release:\"$album\" AND artist:\"$artist\"" \
  --data-urlencode 'fmt=json' \
  -H 'User-Agent: NAS-Stack-Media-Organizer/1.0 (local)' | jq .
```

Если MusicBrainz недоступен, разрешено использовать теги `ffprobe`
(`artist`, `album`, `date`, `track`) и имя каталога, но результат помечай
`confidence=medium` и показывай в dry-run.

## 4. Целевая структура

Новые видео:

```
Movies/<Title (Year)>/<Title (Year)>.<ext>
TV/<Title (Year)>/Season NN/<Title> - SxxExx - <Episode>.<ext>
```

Музыка (сохраняй весь релиз):

```
Music/<Artist>/<Album (Year)>/NN - <Title>.<ext>
Music/<Artist>/<Album (Year)>/cover.jpg
Music/<Artist>/<Album (Year)>/*.cue|*.log|*.m3u8
```

Если в библиотеке уже принят другой стиль для конкретного сериала/фильма,
следуй ему. Не создавай второй вариант той же сущности.

## 5. Проверка перед перемещением

1. Убедись, что источник существует и не является symlink за пределы
   `LIBRARY_ROOT`: `realpath -e`.
2. Проверь свободное место: `df -h "$LIBRARY_ROOT"`.
3. Проверь активные торренты:

```bash
curl -fsS http://localhost:8080/torrent/stats | jq .
```

Если путь источника встречается в активной раздаче — пропусти.
4. Проверь, что цель не существует. При совпадении:
   - не перезаписывай;
   - сравни длительность/размер/`sha256`;
   - при сомнении — `_Review`.
5. Сделай manifest **до** `mv`.

## 6. Применение

Только после подтверждения:

```bash
mkdir -p "$target_dir"
mv -n -- "$source" "$target_dir/"
```

`mv -n` не перезаписывает существующие файлы. Для переноса между разными
файловыми системами используй `cp -a --no-clobber`, затем проверь `sha256`
и только после успеха перемести исходник в `trash` (с подтверждением).

После переноса:

```bash
ffprobe -v error -show_entries format=duration -of default=nw=1 "$target"
```

Для музыки проверь, что перенесены все файлы релиза, а `cover.jpg` и `.cue`
не потерялись.

## 7. Отчёт

В конце выведи таблицу:

| Источник | Тип | Совпадение | Уверенность | Цель | Статус |
|---|---|---|---|---|---|

И отдельно:
- пропущено из-за активных раздач;
- отправлено в `_Review` и почему;
- ошибки TMDb/MusicBrainz;
- сколько файлов перенесено и сколько байт;
- путь к manifest.

## 8. Критерии безопасности

- Не выполняй `rm`, `find ... -delete`, `mv` без `-n` и массовые `chmod/chown`.
- Не трогай `.env`, `docker-compose.yml`, systemd-юниты и базы данных.
- Не запускай переиндексацию библиотеки, пока не завершён перенос.
- Если сомневаешься — остановись и оставь файл в `New_Stuff` с записью в отчёте.
