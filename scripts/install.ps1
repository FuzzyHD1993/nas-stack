# NAS-Stack — Windows installer (WSL2 + Docker Desktop)
$ErrorActionPreference = "Stop"
Write-Host "NAS-Stack installer (Windows/WSL2)" -ForegroundColor Cyan
try { wsl --version | Out-Null } catch { Write-Host "WSL2 не установлен: wsl --install" -ForegroundColor Red; exit 1 }
try { docker --version | Out-Null } catch { Write-Host "Docker Desktop не найден: https://docker.com" -ForegroundColor Red; exit 1 }
$wslHome = (wsl -e bash -c 'echo $HOME').Trim()
$repo = "$wslHome/nas-stack"
if ((wsl -e test -d "$repo"; $LASTEXITCODE) -eq 0) {
    wsl -e bash -c "cd '$repo' && git pull"
} else {
    wsl -e bash -c "cd '$wslHome' && git clone https://github.com/FuzzyHD1993/nas-stack.git nas-stack"
}
wsl -e bash -c "cd '$repo' && bash scripts/install.sh"
Write-Host "Готово: http://localhost:8090/setup" -ForegroundColor Green
