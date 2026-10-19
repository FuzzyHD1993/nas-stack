# Установка на TrueNAS SCALE (24.04+)

- **Runtime:** Docker (встроенный)
- **Пути:** `/mnt/<pool>/<dataset>/`
- **systemd:** есть

## Шаги
1. SSH на TrueNAS.
2. `cd /mnt/<pool>/apps && git clone <repo> nas-stack && cd nas-stack`.
3. `bash scripts/install.sh`.
4. `http://<truenas-ip>:8090/setup`.

Предпочтителен **NFS** для шаринга библиотеки.
