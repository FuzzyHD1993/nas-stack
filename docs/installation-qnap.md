# Установка на QNAP (QTS 5.x)

- **Runtime:** Docker (Container Station)
- **Пути:** `/share/CACHEDEV1_DATA/homes/<user>/`
- **systemd:** нет → хост-демоны в Docker (`--profile nas-non-systemd`)

## Шаги
1. App Center → **Container Station**.
2. SSH: `ssh admin@<qnap-ip>`.
3. `cd /share/CACHEDEV1_DATA/homes && git clone <repo> nas-stack && cd nas-stack`.
4. `bash scripts/install.sh` — автоопределит QNAP/Docker/без systemd.
5. Откройте `http://<qnap-ip>:8090/setup`.

`host-agent` и `books-opener` — отдельные проекты; на QNAP необязательны.
