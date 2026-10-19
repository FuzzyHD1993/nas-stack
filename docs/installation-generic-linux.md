# Установка на Linux (Ubuntu/Debian/Arch/Fedora)

- **Runtime:** Podman или Docker (автоопределение)
- **Пути:** `/mnt/...` или `/home/<user>/...`
- **systemd:** есть → демоны как user-юниты

```bash
# Ubuntu/Debian
sudo apt install podman podman-compose   # или docker.io docker-compose-plugin
# Arch
sudo pacman -S podman
# Fedora
sudo dnf install podman

git clone <repo> nas-stack && cd nas-stack
bash scripts/install.sh
# http://localhost:8090/setup
```
