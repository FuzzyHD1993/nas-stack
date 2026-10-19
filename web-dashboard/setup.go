package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
)

// envPath — путь к .env, который читает/пишет setup-wizard.
func envPath() string {
	return platform.GetEnv("ENV_PATH", "/app/.env")
}

// generateSecret создаёт случайный 32-байтный hex-секрет для JWT_SECRET.
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// needsSetup — нужен ли первичный wizard (нет .env или не заполнены обязательные).
func needsSetup() bool {
	info, err := os.Stat(envPath())
	if err != nil {
		return true
	}
	// bind-mount мог создать каталог вместо файла
	if info.IsDir() {
		return true
	}
	data, err := os.ReadFile(envPath())
	if err != nil || len(data) == 0 {
		return true
	}
	content := string(data)
	for _, key := range []string{"LIBRARY_ROOT", "HOST_IP"} {
		found := false
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, key+"=") {
				found = true
				if strings.TrimSpace(strings.TrimPrefix(line, key+"=")) == "" {
					return true
				}
			}
		}
		if !found {
			return true
		}
	}
	return false
}

func setupRoutes(app *fiber.App) {
	// GET /api/setup/detect — окружение для wizard.
	app.Get("/api/setup/detect", func(c fiber.Ctx) error {
		lib := ""
		if b, err := os.ReadFile(envPath()); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "LIBRARY_ROOT=") {
					lib = strings.TrimSpace(strings.TrimPrefix(line, "LIBRARY_ROOT="))
				}
			}
		}
		if lib == "" {
			lib = platform.GetEnv("LIBRARY_ROOT", "/mnt")
		}
		return c.JSON(fiber.Map{
			"NAS_TYPE":          platform.GetEnv("NAS_TYPE", "generic-linux"),
			"DEFAULT_PATH":      lib,
			"CONTAINER_RUNTIME": platform.GetEnv("CONTAINER_RUNTIME", "unknown"),
			"HAS_SYSTEMD":       platform.GetEnv("HAS_SYSTEMD", "false"),
			"USER_UID":          platform.GetEnv("PUID", "1000"),
			"USER_GID":          platform.GetEnv("PGID", "1000"),
		})
	})

	// GET /api/setup/scan-paths — быстрый поиск папок с медиа (без глубокого обхода).
	app.Get("/api/setup/scan-paths", func(c fiber.Ctx) error {
		found := map[string]string{}
		classify := func(name string) string {
			n := strings.ToLower(name)
			switch {
			case strings.Contains(n, "movie"), strings.Contains(n, "фильм"), strings.Contains(n, "video"):
				return "movies"
			case strings.Contains(n, "music"), strings.Contains(n, "музык"):
				return "music"
			case strings.Contains(n, "book"), strings.Contains(n, "книг"):
				return "books"
			case strings.Contains(n, "photo"), strings.Contains(n, "фото"):
				return "photos"
			}
			return ""
		}
		candidates := []string{}
		// подсказка из LIBRARY_ROOT (.env)
		if b, err := os.ReadFile(envPath()); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "LIBRARY_ROOT=") {
					if v := strings.TrimSpace(strings.TrimPrefix(line, "LIBRARY_ROOT=")); v != "" {
						candidates = append(candidates, v)
					}
				}
			}
		}
		for _, r := range []string{"/mnt", "/media", "/volume1", "/share"} {
			candidates = append(candidates, r)
		}
		if h := os.Getenv("HOME"); h != "" {
			candidates = append(candidates, h)
		}

		var scan func(dir string, depth int)
		scan = func(dir string, depth int) {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				p := filepath.Join(dir, e.Name())
				if kind := classify(e.Name()); kind != "" && found[kind] == "" {
					found[kind] = p
				}
				if depth > 0 && len(found) < 4 {
					scan(p, depth-1)
				}
			}
		}
		for _, cand := range candidates {
			if len(found) >= 4 {
				break
			}
			if _, err := os.Stat(cand); err == nil {
				scan(cand, 1)
			}
		}
		return c.JSON(found)
	})

	// POST /api/setup/reset — удалить .env и вернуться к мастеру.
	app.Post("/api/setup/reset", func(c fiber.Ctx) error {
		if err := os.Remove(envPath()); err != nil && !os.IsNotExist(err) {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "reset"})
	})

	app.Get("/setup", func(c fiber.Ctx) error {
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(setupPage)
	})

	// POST /api/setup/check-path — существует ли путь и сколько в нём элементов.
	app.Post("/api/setup/check-path", func(c fiber.Ctx) error {
		var req struct {
			Path string `json:"path"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "bad json"})
		}
		if strings.TrimSpace(req.Path) == "" {
			return c.Status(400).JSON(fiber.Map{"error": "path required"})
		}
		info, err := os.Stat(req.Path)
		if err != nil {
			return c.JSON(fiber.Map{"exists": false, "error": err.Error()})
		}
		if !info.IsDir() {
			return c.JSON(fiber.Map{"exists": false, "error": "not a directory"})
		}
		entries, err := os.ReadDir(req.Path)
		if err != nil {
			return c.JSON(fiber.Map{"exists": true, "is_dir": true, "items_count": 0, "error": err.Error()})
		}
		return c.JSON(fiber.Map{"exists": true, "is_dir": true, "items_count": len(entries)})
	})

	// POST /api/setup/save — сгенерировать .env из параметров wizard.
	app.Post("/api/setup/save", func(c fiber.Ctx) error {
		var req struct {
			LibraryRoot   string `json:"library_root"`
			MoviesDir     string `json:"movies_dir"`
			TVDir         string `json:"tv_dir"`
			MusicDir      string `json:"music_dir"`
			BooksDir      string `json:"books_dir"`
			YoutubeDir    string `json:"youtube_dir"`
			PhotosDir     string `json:"photos_dir"`
			HostIP        string `json:"host_ip"`
			NasHost       string `json:"nas_host"`
			MPDAddr       string `json:"mpd_addr"`
			TMDbKey       string `json:"tmdb_key"`
			ProwlarrKey   string `json:"prowlarr_key"`
			ProwlarrAuto  bool   `json:"prowlarr_auto"`
			FlibustaLocal bool   `json:"flibusta_local"`
			FlibustaURL   string `json:"flibusta_local_url"`
			JWTSecret     string `json:"jwt_secret"`
			AuthPassword  string `json:"auth_password"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "bad json"})
		}
		if req.LibraryRoot == "" {
			return c.Status(400).JSON(fiber.Map{"error": "library_root required"})
		}
		if req.HostIP == "" {
			return c.Status(400).JSON(fiber.Map{"error": "host_ip required"})
		}
		if _, err := os.Stat(req.LibraryRoot); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": fmt.Sprintf("library_root not found: %v", err)})
		}
		if req.MoviesDir == "" {
			req.MoviesDir = "Movies"
		}
		if req.TVDir == "" {
			req.TVDir = "TV"
		}
		if req.MusicDir == "" {
			req.MusicDir = "Music"
		}
		if req.BooksDir == "" {
			req.BooksDir = "Books"
		}
		if req.YoutubeDir == "" {
			req.YoutubeDir = "YT_video"
		}
		if req.PhotosDir == "" {
			req.PhotosDir = "Photos"
		}
		if req.MPDAddr == "" {
			req.MPDAddr = "host.containers.internal:6600"
		}
		if req.JWTSecret == "" {
			secret, err := generateSecret()
			if err != nil {
				return c.Status(500).JSON(fiber.Map{"error": "rng: " + err.Error()})
			}
			req.JWTSecret = secret
		}

		content := fmt.Sprintf(`# NAS Stack — сгенерировано Setup Wizard
# Дата: %s

# ---------- Пути ----------
LIBRARY_ROOT=%s
MEDIA_MOVIES_HOST_DIR=%s/%s
MEDIA_TV_HOST_DIR=%s/%s
MUSIC_HOST_DIR=%s/%s
BOOKS_HOST_DIR=%s/%s
YOUTUBE_HOST_DIR=%s/%s
MEDIA_PHOTOS_HOST_DIR=%s/%s

# ---------- Сеть ----------
HOST_IP=%s
NAS_HOST=%s
MPD_ADDR=%s

# ---------- API-ключи ----------
TMDB_API_KEY=%s
PROWLARR_API_KEY=%s
JWT_SECRET=%s
AUTH_PASSWORD=%s
AUTH_ENABLED=%t

# ---------- Дефолты ----------
TMDB_LANG=ru-RU
POSTGRES_URL=postgres://nas:nas_secret@postgres:5432/nas_history?sslmode=disable
REDIS_ADDR=redis:6379
HTTP_PROXY=
DISK_WARN_PERCENT=80
DISK_CRIT_PERCENT=90
BACKUP_KEEP=7
BIT_PERFECT_ENABLED=false
`,
			time.Now().Format("2006-01-02 15:04:05"),
			req.LibraryRoot,
			req.LibraryRoot, req.MoviesDir,
			req.LibraryRoot, req.TVDir,
			req.LibraryRoot, req.MusicDir,
			req.LibraryRoot, req.BooksDir,
			req.LibraryRoot, req.YoutubeDir,
			req.LibraryRoot, req.PhotosDir,
			req.HostIP,
			req.NasHost,
			req.MPDAddr,
			req.TMDbKey,
			req.ProwlarrKey,
			req.JWTSecret,
			req.AuthPassword,
			req.AuthPassword != "",
		)

		updates := map[string]string{
			"LIBRARY_ROOT":          req.LibraryRoot,
			"MEDIA_MOVIES_HOST_DIR": req.LibraryRoot + "/" + req.MoviesDir,
			"MEDIA_TV_HOST_DIR":     req.LibraryRoot + "/" + req.TVDir,
			"MUSIC_HOST_DIR":        req.LibraryRoot + "/" + req.MusicDir,
			"BOOKS_HOST_DIR":        req.LibraryRoot + "/" + req.BooksDir,
			"YOUTUBE_HOST_DIR":      req.LibraryRoot + "/" + req.YoutubeDir,
			"MEDIA_PHOTOS_HOST_DIR": req.LibraryRoot + "/" + req.PhotosDir,
			"HOST_IP":               req.HostIP,
			"NAS_HOST":              req.NasHost,
			"MPD_ADDR":              req.MPDAddr,
			"TMDB_API_KEY":          req.TMDbKey,
			"PROWLARR_API_KEY":      req.ProwlarrKey,
			"JWT_SECRET":            req.JWTSecret,
			"AUTH_PASSWORD":         req.AuthPassword,
			"AUTH_ENABLED":          fmt.Sprintf("%t", req.AuthPassword != ""),
		}

		path := envPath()
		if _, statErr := os.Stat(path); statErr == nil {
			// Существующий .env — обновляем только ключи wizard, остальное сохраняем.
			if old, err := os.ReadFile(path); err == nil {
				_ = os.WriteFile(path+".bak."+time.Now().Format("20060102-150405"), old, 0o644)
			}
			if err := mergeEnv(path, updates); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": "write failed: " + err.Error()})
			}
		} else {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": "write failed: " + err.Error()})
			}
		}
		// Авто-настройка Prowlarr (асинхронно, чтобы не блокировать ответ).
		if req.ProwlarrAuto {
			go func() {
				target := platform.GetEnv("SCHEDULER_URL", "http://scheduler-service:8084") + "/prowlarr/setup"
				payload := fmt.Sprintf(`{"api_key":%q}`, req.ProwlarrKey)
				hc := &http.Client{Timeout: 60 * time.Second}
				resp, err := hc.Post(target, "application/json", strings.NewReader(payload))
				if err != nil {
					log.Printf("⚠️ Prowlarr auto-setup: %v", err)
					return
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				log.Printf("✅ Prowlarr auto-setup: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
			}()
		}
		return c.JSON(fiber.Map{"success": true, "message": ".env сохранён", "path": path})
	})
}

// mergeEnv обновляет/добавляет ключи в существующем .env, сохраняя остальные строки.
func mergeEnv(path string, updates map[string]string) error {
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(old), "\n")
	seen := map[string]bool{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if v, ok := updates[key]; ok {
			lines[i] = key + "=" + v
			seen[key] = true
		}
	}
	for k, v := range updates {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return os.WriteFile(path, []byte(out), 0o644)
}

// setupPage — отдельная страница первичной настройки (не зависит от основного UI).
const setupPage = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>NAS Stack — Первичная настройка</title>
<style>
:root{--bg:#1e1e2e;--bg-card:#313244;--bg-button:#45475a;--bg-button-hover:#585b70;--fg:#cdd6f4;--fg-muted:#a6adc8;--fg-dim:#6c7086;--accent:#89b4fa;--success:#a6e3a1;--error:#f38ba8;--warning:#f9e2af}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,-apple-system,sans-serif;background:var(--bg);color:var(--fg);min-height:100vh;display:flex;align-items:center;justify-content:center;padding:20px}
.setup-container{background:var(--bg-card);border-radius:16px;padding:40px;max-width:720px;width:100%;box-shadow:0 10px 40px rgba(0,0,0,.4)}
.setup-header{text-align:center;margin-bottom:24px}
.setup-header h1{font-size:26px;margin-bottom:6px}
.setup-header p{color:var(--fg-muted);font-size:14px}
.step{display:none}.step.active{display:block}
.step-title{font-size:18px;font-weight:600;margin-bottom:18px;padding-bottom:10px;border-bottom:1px solid var(--bg-button)}
.form-group{margin-bottom:16px}
.form-group label{display:block;margin-bottom:6px;font-size:13px;color:var(--fg-muted)}
.form-group label .required{color:var(--error)}
.form-group input{width:100%;padding:11px 13px;background:var(--bg);border:1px solid var(--bg-button);border-radius:8px;color:var(--fg);font-size:14px;font-family:inherit}
.form-group input:focus{outline:none;border-color:var(--accent)}
.form-group .hint{font-size:12px;color:var(--fg-dim);margin-top:5px}
.btn{padding:11px 22px;background:var(--accent);color:var(--bg);border:none;border-radius:8px;font-size:14px;font-weight:600;cursor:pointer}
.btn:disabled{opacity:.5;cursor:not-allowed}
.btn-secondary{background:var(--bg-button);color:var(--fg)}
.btn-row{display:flex;justify-content:space-between;gap:12px;margin-top:26px}
.progress-bar{height:4px;background:var(--bg-button);border-radius:2px;margin-bottom:26px;overflow:hidden}
.progress-bar-fill{height:100%;background:var(--accent);transition:width .3s}
.check-item{display:flex;align-items:center;gap:10px;padding:11px;background:var(--bg);border-radius:8px;margin-bottom:8px;font-size:13px;word-break:break-all}
.check-item.ok{border-left:3px solid var(--success)}.check-item.fail{border-left:3px solid var(--error)}.check-item.warn{border-left:3px solid var(--warning)}
.alert{padding:11px 14px;border-radius:8px;margin-bottom:14px;font-size:13px}
.alert-error{background:rgba(243,139,168,.15);border-left:3px solid var(--error);color:var(--error)}
.alert-success{background:rgba(166,227,161,.15);border-left:3px solid var(--success);color:var(--success)}
.alert-info{background:rgba(137,180,250,.15);border-left:3px solid var(--accent);color:var(--accent)}
pre{background:var(--bg);padding:14px;border-radius:8px;overflow-x:auto;font-size:13px;color:var(--accent)}
</style>
</head>
<body>
<div class="setup-container">
  <div class="setup-header"><h1>🖥️ NAS Stack</h1><p>Первичная настройка приложения</p><div id="nasInfo" style="margin-top:6px;font-size:12px;color:var(--fg-dim);"></div></div>
  <div class="progress-bar"><div class="progress-bar-fill" id="progress" style="width:0%"></div></div>

  <div class="step active" data-step="1">
    <div class="step-title">📁 Шаг 1: Пути к библиотеке</div>
    <div class="form-group"><label>Корень библиотеки <span class="required">*</span></label>
      <input id="libraryRoot" value="/mnt/nas/root/FuzzyHD"><div class="hint" id="libraryRootHint">Где смонтирован NAS / корень медиатеки</div></div>
    <div class="form-group"><label>Способ маунта</label>
      <select id="mountType" onchange="generateFstabEntry()" style="width:100%;padding:11px;background:var(--bg);border:1px solid var(--bg-button);border-radius:8px;color:var(--fg);">
        <option value="local">Локально (уже смонтировано)</option>
        <option value="cifs">CIFS/SMB</option>
        <option value="nfs">NFS</option>
      </select>
    </div>
    <div class="form-group" id="mountSourceRow" style="display:none;">
      <label>Сервер и шара</label>
      <div style="display:flex;gap:8px;">
        <input id="mountServer" placeholder="192.168.0.5" oninput="generateFstabEntry()" style="flex:1;">
        <input id="mountShare" placeholder="homes" oninput="generateFstabEntry()" style="flex:1;">
      </div>
      <div class="hint">Пример fstab-строки:</div>
      <pre id="fstabPreview" style="white-space:pre-wrap;">—</pre>
    </div>
    <div class="form-group"><label>Фильмы</label><input id="moviesDir" value="Movies"><div class="hint">Относительно корня</div></div>
    <div class="form-group"><label>Сериалы</label><input id="tvDir" value="TV"></div>
    <div class="form-group"><label>Музыка</label><input id="musicDir" value="Music"></div>
    <div class="form-group"><label>Книги</label><input id="booksDir" value="Books"></div>
    <div class="form-group"><label>YouTube</label><input id="youtubeDir" value="YT_video"></div>
    <div class="form-group"><label>Фото</label><input id="photosDir" value="Photos"></div>
    <div class="form-group">
      <button type="button" class="btn btn-secondary" onclick="autoScan(this)">🔍 Найти мои папки</button>
      <div id="scanResult" style="margin-top:8px;"></div>
    </div>
  </div>

  <div class="step" data-step="2">
    <div class="step-title">🌐 Шаг 2: Сеть</div>
    <div class="form-group"><label>IP хоста <span class="required">*</span></label><input id="hostIP" value="192.168.0.4"></div>
    <div class="form-group"><label>IP NAS</label><input id="nasHost" value="192.168.0.5"></div>
    <div class="form-group"><label>MPD адрес</label><input id="mpdAddr" value="host.containers.internal:6600"><div class="hint">Если MPD на хосте — оставьте как есть</div></div>
  </div>

  <div class="step" data-step="3">
    <div class="step-title">🔑 Шаг 3: API-ключи</div>
    <div class="alert alert-info">ℹ️ Ключи опциональны — можно добавить позже в .env.</div>
    <div class="form-group"><label>TMDb API Key</label><input id="tmdbKey" placeholder="a88d5639..."><div class="hint">themoviedb.org/settings/api</div></div>
    <div class="form-group"><label>Prowlarr API Key</label><input id="prowlarrKey" placeholder="a147e44d..."><div class="hint">Prowlarr → Settings → General → API Key</div></div>
    <div class="form-group">
      <label style="display:flex;align-items:center;gap:8px;cursor:pointer;">
        <input type="checkbox" id="prowlarrAuto" checked style="width:auto;">
        Настроить Prowlarr автоматически
      </label>
      <div class="hint">Добавит 1337x, The Pirate Bay, Byrutor (если ещё не добавлены)</div>
    </div>
    <div class="form-group">
      <label style="display:flex;align-items:center;gap:8px;cursor:pointer;">
        <input type="checkbox" id="flibustaLocal" style="width:auto;">
        Использовать локальную библиотеку Флибусты (опционально)
      </label>
      <div class="hint">Профиль <code>flibusta-local</code> (~200–550 ГБ). <a href="https://github.com/FuzzyHD1993/nas-stack/blob/main/docs/local-flibusta.md" target="_blank">Инструкция</a>.</div>
      <input id="flibustaLocalURL" value="http://flibusta-local:27100" style="margin-top:6px;">
    </div>
    <div class="form-group"><label>JWT Secret</label><input id="jwtSecret" placeholder="нажмите «Сгенерировать»">
      <div class="hint"><button type="button" class="btn btn-secondary" onclick="generateJWT()" style="padding:4px 12px;font-size:12px;margin-top:6px;">🎲 Сгенерировать</button></div></div>
    <div class="form-group"><label>Пароль входа (AUTH_PASSWORD)</label><input id="authPassword" placeholder="пусто = без пароля"></div>
  </div>

  <div class="step" data-step="4">
    <div class="step-title">✅ Шаг 4: Проверка путей</div>
    <div id="checks"><div class="check-item"><span class="icon">⏳</span><span class="text">Проверка...</span></div></div>
  </div>

  <div class="step" data-step="5">
    <div class="step-title">🎉 Готово!</div>
    <div id="finalStatus"></div>
    <p style="color:var(--fg-muted);margin:14px 0 8px;">Запустите/перезапустите стек:</p>
    <pre>podman compose up -d --force-recreate</pre>
    <p style="margin-top:14px;"><a href="/" style="color:var(--accent);">Перейти в дашборд →</a></p>
  </div>

  <div class="btn-row">
    <button class="btn btn-secondary" id="btnBack" onclick="prevStep()" style="visibility:hidden;">← Назад</button>
    <button class="btn" id="btnNext" onclick="nextStep()">Далее →</button>
  </div>
</div>
<script>
var currentStep = 1;
var totalSteps = 5;

function updateProgress(){var pct=((currentStep-1)/(totalSteps-1))*100;document.getElementById('progress').style.width=pct+'%';}
function showStep(n){
  document.querySelectorAll('.step').forEach(function(el){el.classList.remove('active');});
  document.querySelector('.step[data-step="'+n+'"]').classList.add('active');
  document.getElementById('btnBack').style.visibility = n>1?'visible':'hidden';
  var b=document.getElementById('btnNext');
  if(n===totalSteps){b.style.display='none';}
  else if(n===4){b.style.display='inline-block';b.textContent='Завершить настройку';}
  else {b.style.display='inline-block';b.textContent='Далее →';}
  updateProgress();
}
function prevStep(){if(currentStep>1){currentStep--;showStep(currentStep);}}
async function nextStep(){
  if(currentStep===4){await saveConfig();return;}
  currentStep++;showStep(currentStep);
  if(currentStep===4){await runChecks();}
}
function collectConfig(){
  return {
    library_root:document.getElementById('libraryRoot').value.trim(),
    movies_dir:document.getElementById('moviesDir').value.trim(),
    tv_dir:document.getElementById('tvDir').value.trim(),
    music_dir:document.getElementById('musicDir').value.trim(),
    books_dir:document.getElementById('booksDir').value.trim(),
    youtube_dir:document.getElementById('youtubeDir').value.trim(),
    photos_dir:document.getElementById('photosDir').value.trim(),
    host_ip:document.getElementById('hostIP').value.trim(),
    nas_host:document.getElementById('nasHost').value.trim(),
    mpd_addr:document.getElementById('mpdAddr').value.trim(),
    tmdb_key:document.getElementById('tmdbKey').value.trim(),
    prowlarr_key:document.getElementById('prowlarrKey').value.trim(),
    prowlarr_auto:document.getElementById('prowlarrAuto').checked,
    flibusta_local:document.getElementById('flibustaLocal').checked,
    flibusta_local_url:document.getElementById('flibustaLocalURL').value.trim(),
    jwt_secret:document.getElementById('jwtSecret').value.trim(),
    auth_password:document.getElementById('authPassword').value.trim()
  };
}
async function runChecks(){
  var cfg=collectConfig();
  var checks=[
    {name:'Корень библиотеки',path:cfg.library_root,critical:true},
    {name:'Фильмы',path:cfg.library_root+'/'+cfg.movies_dir,critical:false},
    {name:'Сериалы',path:cfg.library_root+'/'+cfg.tv_dir,critical:false},
    {name:'Музыка',path:cfg.library_root+'/'+cfg.music_dir,critical:false},
    {name:'Книги',path:cfg.library_root+'/'+cfg.books_dir,critical:false}
  ];
  var el=document.getElementById('checks');var html='';
  checks.forEach(function(c){html+='<div class="check-item" data-path="'+c.path+'"><span class="icon">⏳</span><span class="text">'+c.name+': '+c.path+'</span></div>';});
  el.innerHTML=html;
  for(var i=0;i<checks.length;i++){
    var c=checks[i];
    var row=document.querySelector('.check-item[data-path="'+c.path+'"]');
    try{
      var r=await fetch('/api/setup/check-path',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:c.path})});
      var d=await r.json();
      if(d.exists){row.classList.add('ok');row.querySelector('.icon').textContent='✅';row.querySelector('.text').textContent+=' ('+d.items_count+' элементов)';}
      else{row.classList.add(c.critical?'fail':'warn');row.querySelector('.icon').textContent=c.critical?'❌':'⚠️';row.querySelector('.text').textContent+=' — '+(d.error||'не найдено');}
    }catch(e){row.classList.add('warn');row.querySelector('.icon').textContent='⚠️';row.querySelector('.text').textContent+=' — не удалось проверить';}
  }
}
async function saveConfig(){
  var cfg=collectConfig();var btn=document.getElementById('btnNext');
  btn.disabled=true;btn.textContent='Сохранение...';
  try{
    var r=await fetch('/api/setup/save',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(cfg)});
    var d=await r.json();
    currentStep=5;showStep(5);
    if(d.success){document.getElementById('finalStatus').innerHTML='<div class="alert alert-success">✅ .env сохранён: '+d.path+'</div>';}
    else{document.getElementById('finalStatus').innerHTML='<div class="alert alert-error">❌ '+(d.error||'Ошибка сохранения')+'</div>';}
  }catch(e){document.getElementById('finalStatus').innerHTML='<div class="alert alert-error">❌ Сетевая ошибка: '+e.message+'</div>';}
}
function generateJWT(){
  var b=new Uint8Array(32);crypto.getRandomValues(b);
  var hex=Array.from(b).map(function(x){return x.toString(16).padStart(2,'0');}).join('');
  document.getElementById('jwtSecret').value=hex;
}
async function autoScan(btn){
  if(btn){btn.disabled=true;btn.textContent='⏳ Ищу...';}
  var res=document.getElementById('scanResult');
  try{
    var r=await fetch('/api/setup/scan-paths');var d=await r.json();
    var map={movies:'moviesDir',music:'musicDir',books:'booksDir',photos:'photosDir'};
    var n=0;
    Object.keys(map).forEach(function(k){
      if(d[k]){var el=document.getElementById(map[k]);if(el){el.value=d[k];n++;}}
    });
    if(res)res.innerHTML='<div class="alert alert-success">✅ Найдено папок: '+n+'. Проверьте поля.</div>';
  }catch(e){if(res)res.innerHTML='<div class="alert alert-error">❌ Ошибка поиска</div>';}
  finally{if(btn){btn.disabled=false;btn.textContent='🔍 Найти мои папки';}}
}
var detected={};
async function detectEnvironment(){
  try{
    var r=await fetch('/api/setup/detect');detected=await r.json();
    var root=document.getElementById('libraryRoot');
    if(detected.DEFAULT_PATH){root.value=detected.DEFAULT_PATH;root.placeholder=detected.DEFAULT_PATH;}
    var ni=document.getElementById('nasInfo');
    if(ni)ni.textContent='Обнаружено: '+(detected.NAS_TYPE||'linux')+' · '+(detected.CONTAINER_RUNTIME||'runtime?')+' · systemd: '+(detected.HAS_SYSTEMD||'?');
    updatePathHints(detected.NAS_TYPE);
  }catch(e){}
}
function updatePathHints(nas){
  var hints={synology:'/volume1/homes/<user>/',qnap:'/share/CACHEDEV1_DATA/homes/<user>/',truenas:'/mnt/<pool>/<dataset>/',unraid:'/mnt/user/<share>/',openmediavault:'/srv/<share>/',macos:'/Volumes/<share>/',wsl2:'~/media',"generic-linux":'/mnt/<drive>/ или /home/<user>/'};
  var h=document.getElementById('libraryRootHint');if(h)h.textContent='Рекомендуется: '+(hints[nas]||hints['generic-linux']);
}
function generateFstabEntry(){
  var type=document.getElementById('mountType').value;
  var row=document.getElementById('mountSourceRow');
  if(row)row.style.display=(type==='local')?'none':'';
  var srv=document.getElementById('mountServer').value;
  var shr=document.getElementById('mountShare').value;
  var tgt=document.getElementById('libraryRoot').value;
  var out='—';
  if(type==='cifs'&&srv&&shr)out='//'+srv+'/'+shr+' '+tgt+' cifs credentials=/etc/smbcredentials,vers=3.0,uid='+(detected.USER_UID||1000)+',gid='+(detected.USER_GID||1000)+',soft,nofail,x-systemd.automount,_netdev 0 0';
  if(type==='nfs'&&srv&&shr)out=srv+':/'+shr+' '+tgt+' nfs4 defaults,soft,nofail,x-systemd.automount,_netdev 0 0';
  document.getElementById('fstabPreview').textContent=out;
}
detectEnvironment();
updateProgress();
</script>
</body>
</html>`
