# Установка на Windows (WSL2)

1. `wsl --install` (PowerShell от администратора), перезагрузка.
2. Установите **Docker Desktop** с WSL2-backend.
3. В WSL: `git clone <repo> nas-stack && cd nas-stack`.
4. `bash scripts/install.sh` (auto-detect WSL2 → Docker, без systemd).
5. `http://localhost:8090/setup`.

⚠️ Храните библиотеку **внутри WSL2** (`~/media`) — пути `C:\` через `/mnt/c` медленные.
Опционально: `powershell -File scripts/install.ps1`.
