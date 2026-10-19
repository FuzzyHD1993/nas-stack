# NAS Host Agent

Часть монорепозитория `nas-stack` (`host-agent/`); также собирается в Docker/GoReleaser.

Лёгкий HTTP-агент на хосте (:9998) для данных, доступных только из хост-окружения:
температуры (`sensors`), SMART (`smartctl` через `sudo -n`), диски, процессы,
сеть, GPU (`nvidia-smi`) и очистка диска (dry-first).

## Эндпоинты

| Метод | Путь | Назначение |
|-------|------|-----------|
| GET | `/health` | статус, hostname, kernel |
| GET | `/metrics` | Prometheus |
| GET | `/sensors` | температуры CPU/NVMe (sensors -j) |
| GET | `/smart` | SMART дисков (health, temp, wear, power-on) |
| GET | `/disks` | реальные ФС (df, whitelist, дедуп) |
| GET | `/processes?limit=10` | топ процессов по CPU |
| GET | `/network?iface=eno1` | rx/tx, errors, drops |
| GET | `/uptime`, `/load`, `/gpu` | аптайм, loadavg, GPU |
| GET | `/cleanup/preview` | что можно очистить (размеры) |
| POST | `/cleanup/run?dry=false` | очистка (podman, journald, cache, trash, pacman) |

## Сборка и запуск

```bash
go build -o nas-host-agent .
./nas-host-agent        # слушает 0.0.0.0:9998
```

## Установка systemd-юнита

```bash
./scripts/install-systemd.sh
systemctl --user enable --now nas-host-agent.service
systemctl --user status nas-host-agent.service --no-pager
```

## Интеграция

`stats-service` проксирует `/sensors`, `/smart`, `/temp`, `/cleanup/*`
(`HOST_AGENT_URL=http://host.containers.internal:9998`). Prometheus job `host-agent`.
