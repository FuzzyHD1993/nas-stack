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
		page, err := staticFS.ReadFile("static/setup.html")
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "setup page not found")
		}
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.Send(page)
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
				httpReq, err := http.NewRequest(http.MethodPost, target, strings.NewReader(payload))
				if err != nil {
					log.Printf("⚠️ Prowlarr auto-setup: %v", err)
					return
				}
				httpReq.Header.Set("Content-Type", "application/json")
				// /prowlarr/setup закрыт ADMIN_TOKEN (раньше был анонимным SSRF).
				if t := strings.TrimSpace(os.Getenv("ADMIN_TOKEN")); t != "" {
					httpReq.Header.Set("X-Admin-Token", t)
				}
				resp, err := hc.Do(httpReq)
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
