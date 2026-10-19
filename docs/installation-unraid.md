# Установка на Unraid (6.12+)

- **Runtime:** Docker (встроенный)
- **Пути:** `/mnt/user/<share>/`
- **systemd:** есть (ограниченно)

```bash
cd /mnt/user/appdata
git clone <repo> nas-stack && cd nas-stack
bash scripts/install.sh
# http://<unraid-ip>:8090/setup
```
