# Установка NAS-Stack

## Требования

- **Linux** (рекомендуется), либо Docker Desktop / WSL2.
- **Podman** (или Docker) + compose-провайдер `docker-compose` (НЕ `podman-compose`).
- **Go 1.27+** — только если собираете сервисы локально (в образах уже есть).
- **8 ГБ RAM** рекомендуется (минимум 4 ГБ).
- **~50 ГБ** свободного места.
- Хост-зависимые компоненты (опционально):
  - **MPD** — для музыки;
  - **ffmpeg** — внутри `media-service` (уже в образе);
  - **nvidia-smi** — если есть NVIDIA GPU;
  - **snapper** — для BTRFS-снапшотов;
  - **nas-host-agent** — для датчиков/SMART/очистки/дубликатов.

## Шаги

### Вариант A: Setup Wizard (рекомендуется)

```bash
git clone git@github.com:FuzzyHD1993/nas-stack.git
cd nas-stack
cp .env.example .env
podman compose up -d
```

Откройте **http://localhost:8090/setup** и пройдите 5 шагов:
**Пути → Сеть → API-ключи → Проверка → Готово**. Wizard проверит существование
путей (`/api/setup/check-path`), сгенерирует `JWT_SECRET` и сохранит `.env`
(старый — в `.env.bak.<дата>`). Затем:

```bash
podman compose up -d --force-recreate
```

Повторно: `http://localhost:8090/?setup=1`.
Авто-редирект при отсутствии/невалидном `.env`: `SETUP_AUTO_REDIRECT=true`.

### Вариант B: вручную

```bash
git clone git@github.com:FuzzyHD1993/nas-stack.git
cd nas-stack
cp .env.example .env
```

Заполните `.env` — обязательно:

| Переменная | Описание | Где взять |
|------------|----------|-----------|
| `NAS_HOST` | IP NAS (Synology/QNAP) | `ip a` в вашей сети |
| `NAS_USER` / `NAS_PASS` | Учётка DownloadStation | Synology |
| `LIBRARY_ROOT` | Корень медиатеки на NAS | ваш путь |
| `TMDB_API_KEY` | Ключ TMDb | themoviedb.org/settings/api |
| `PROWLARR_API_KEY` | Ключ Prowlarr | Prowlarr → Settings → General |
| `JWT_SECRET` | Секрет JWT | `openssl rand -hex 16` |
| `AUTH_PASSWORD` | Пароль входа | придумайте |
| `HOST_IP` | LAN-IP хоста | `ip a` |

Запуск:

```bash
podman compose up -d --build
# UI: http://localhost:8090
```

Проверка:

```bash
bash scripts/smoke-test.sh
```

## Пути и монтирования

Пути `/mnt/nas/...` — это **дефолт**; на другой системе задайте свои через
`NAS_ROOT`, `LIBRARY_ROOT` и `*_HOST_DIR` в `.env`. Контейнерные пути (`/movies`,
`/tv`, `/music`) остаются постоянными.

## systemd (автозапуск)

```bash
./scripts/install-systemd.sh
systemctl --user enable --now nas-stack.service
systemctl --user enable --now nas-notify.service
systemctl --user enable --now nas-stack-watchdog.timer
```

## Обновление

```bash
bash scripts/update-stack.sh     # pull → build → up → health-check → авто-откат
```
