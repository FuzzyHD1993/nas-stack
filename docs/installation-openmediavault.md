# Установка на OpenMediaVault 7

- **Runtime:** Docker (`apt install docker.io docker-compose-plugin`)
- **Пути:** `/srv/<share>/`
- **systemd:** есть

```bash
cd /srv
git clone <repo> nas-stack && cd nas-stack
bash scripts/install.sh
# http://<omv-ip>:8090/setup
```
