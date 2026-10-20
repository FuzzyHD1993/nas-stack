package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// updateRunning — single-flight для /update/run: параллельные git pull/compose
// ломают index.lock и рестартят контейнеры.
var updateRunning atomic.Bool

// runUpdateScript вынесен в переменную для инъекции в тестах:
// тест не должен реально запускать update.sh на CI-хосте.
var runUpdateScript = func(dir, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	return cmd.CombinedOutput()
}

// execOutput — внешняя команда с дедлайном: зависший df/du/smartctl на
// сетевой ФС не должен держать горутину fiber вечно.
func execOutput(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	return cmd.Output()
}

// ifaceRe — Linux IFNAMSIZ=16, без ведущего '-', чтобы не было argument injection.
var ifaceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,14}$`)

// validIface не даёт передать в argv `ip` флаги (-batch) и мусорные имена.
func validIface(s string) bool {
	return ifaceRe.MatchString(s)
}

// newApp собирает приложение агента. Вынесено из main(), потому что на каждый
// адрес прослушивания нужен СВОЙ экземпляр: fiber не умеет обслуживать один
// App двумя слушателями (app.Listener + app.Listen ломают startupProcess).
func newApp() *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "NAS Host Agent",
		BodyLimit:    1 << 20,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  60 * time.Second,
	})

	app.Use(recover.New())
	app.Use(func(c fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Cache-Control", "no-store")
		return c.Next()
	})

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error {
		host, _ := os.Hostname()
		kernel := kernelVersion()
		return c.JSON(fiber.Map{"status": "ok", "service": "host-agent", "hostname": host, "kernel": kernel, "time": time.Now()})
	})

	app.Get("/sensors", requireAdminToken, func(c fiber.Ctx) error {
		out, err := execOutput(5*time.Second, "sensors", "-j")
		if err != nil {
			log.Printf("sensors: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "sensors unavailable"})
		}
		return c.JSON(parseSensors(out))
	})

	app.Get("/smart", requireAdminToken, func(c fiber.Ctx) error {
		return c.JSON(getSmart())
	})

	app.Get("/disks", requireAdminToken, func(c fiber.Ctx) error {
		return c.JSON(getDisks())
	})

	app.Get("/processes", requireAdminToken, func(c fiber.Ctx) error {
		limit, _ := strconv.Atoi(c.Query("limit", "10"))
		if limit <= 0 || limit > 100 {
			limit = 10
		}
		return c.JSON(getProcesses(limit))
	})

	app.Get("/network", requireAdminToken, func(c fiber.Ctx) error {
		iface := strings.TrimSpace(c.Query("iface", "eno1"))
		if !validIface(iface) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid iface"})
		}
		return c.JSON(getNetwork(iface))
	})

	app.Get("/uptime", requireAdminToken, func(c fiber.Ctx) error {
		out, _ := execOutput(3*time.Second, "uptime", "-p")
		return c.JSON(fiber.Map{"uptime": strings.TrimSpace(string(out))})
	})

	app.Get("/load", requireAdminToken, func(c fiber.Ctx) error {
		data, _ := os.ReadFile("/proc/loadavg")
		fields := strings.Fields(string(data))
		return c.JSON(fiber.Map{"raw": strings.TrimSpace(string(data)), "load1": atof(fields, 0), "load5": atof(fields, 1), "load15": atof(fields, 2)})
	})

	app.Get("/gpu", requireAdminToken, func(c fiber.Ctx) error {
		out, err := execOutput(5*time.Second, "nvidia-smi",
			"--query-gpu=name,temperature.gpu,utilization.gpu,memory.used,memory.total",
			"--format=csv,noheader,nounits")
		if err != nil {
			return c.JSON(fiber.Map{"error": "nvidia-smi not available"})
		}
		parts := strings.Split(strings.TrimSpace(string(out)), ", ")
		if len(parts) < 5 {
			return c.JSON(fiber.Map{"error": "unexpected nvidia-smi output"})
		}
		return c.JSON(fiber.Map{
			"name": parts[0], "temp": parts[1] + "°C", "usage": parts[2] + "%",
			"memory_used": parts[3] + " MB", "memory_total": parts[4] + " MB",
		})
	})

	app.Get("/cleanup/preview", requireAdminToken, func(c fiber.Ctx) error {
		return c.JSON(cleanupPreview())
	})

	app.Post("/cleanup/run", requireAdminToken, func(c fiber.Ctx) error {
		dry := c.Query("dry", "true") != "false"

		var body struct {
			Targets      []string `json:"targets"`
			All          bool     `json:"all"`
			AllowKernels bool     `json:"allow_kernels"`
		}
		// Пустое/битое тело больше НЕ означает «удалить всё».
		if len(c.Body()) > 0 {
			if err := c.Bind().JSON(&body); err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad json"})
			}
		}
		if body.AllowKernels {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "allow_kernels is not supported"})
		}
		if body.All && len(body.Targets) > 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "use either targets or all=true"})
		}
		if len(body.Targets) > len(cleanupTargets) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "too many targets"})
		}
		for _, t := range body.Targets {
			if !cleanupTargets[t] {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unknown target: " + t})
			}
		}
		if !body.All && len(body.Targets) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "targets required (or all=true)"})
		}
		if dry {
			return c.JSON(fiber.Map{"dry": true, "preview": cleanupPreview()})
		}
		if !cleanupMu.TryLock() {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "cleanup already running"})
		}
		defer cleanupMu.Unlock()

		return c.JSON(runCleanup(body.Targets, body.All))
	})

	// Перемещение дубликатов в бэкап (медиа смонтировано ro — пишем через агент).
	// ---- Auto-update (git + compose на хосте) ----
	repoDir := func() string {
		return platform.GetEnv("REPO_DIR", "/home/konstantin/projects/nas-stack")
	}
	app.Get("/update/status", requireAdminToken, func(c fiber.Ctx) error {
		dir := repoDir()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		fetch := exec.CommandContext(ctx, "git", "-C", dir, "fetch", "--quiet", "github", "main")
		fetch.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := fetch.CombinedOutput(); err != nil {
			log.Printf("update/status: git fetch: %v: %s", err, redactLog(out))
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "git fetch failed"})
		}

		cur, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "git unavailable"})
		}
		cnt, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-list", "HEAD..github/main", "--count").Output()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "git unavailable"})
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(cnt)))
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "unexpected git output"})
		}
		return c.JSON(fiber.Map{
			"current_commit":    strings.TrimSpace(string(cur)),
			"updates_available": n,
		})
	})
	app.Post("/update/run", requireAdminToken, func(c fiber.Ctx) error {
		dir := strings.TrimSpace(repoDir())
		if !filepath.IsAbs(dir) || filepath.Clean(dir) == string(filepath.Separator) {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "invalid REPO_DIR"})
		}

		script := filepath.Join(dir, "scripts", "update.sh")
		fi, err := os.Stat(script)
		if err != nil || !fi.Mode().IsRegular() {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "update script not found"})
		}

		if !updateRunning.CompareAndSwap(false, true) {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "update already running"})
		}

		go func() {
			defer updateRunning.Store(false)
			out, err := runUpdateScript(dir, script)
			log.Printf("🔄 update: err=%v\n%s", err, redactLog(out))
		}()

		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"status": "started"})
	})

	app.Post("/duplicates/resolve", requireAdminToken, func(c fiber.Ctx) error {
		var req struct {
			Paths []string `json:"paths"`
			Keep  string   `json:"keep"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad json"})
		}
		if len(req.Paths) == 0 || len(req.Paths) > 1000 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid paths"})
		}

		libraryRoot := strings.TrimSpace(platform.GetEnv("LIBRARY_ROOT", "/mnt/nas/root/FuzzyHD"))
		backupDir := strings.TrimSpace(platform.GetEnv("DUPLICATES_BACKUP_DIR", "/mnt/nas/Backups/duplicates"))

		if libraryRoot == "" || filepath.Clean(libraryRoot) == string(filepath.Separator) {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "invalid LIBRARY_ROOT"})
		}
		if _, err := os.Stat(libraryRoot); err != nil {
			log.Printf("duplicates: library root %s: %v", libraryRoot, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "library unavailable"})
		}
		rootAbs, err := filepath.Abs(filepath.Clean(libraryRoot))
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "invalid LIBRARY_ROOT"})
		}

		backupDir, err = filepath.Abs(filepath.Clean(backupDir))
		if err != nil || backupDir == string(filepath.Separator) {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "invalid DUPLICATES_BACKUP_DIR"})
		}
		if rel, err := filepath.Rel(rootAbs, backupDir); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "backup dir must be outside LIBRARY_ROOT"})
		}
		if err := os.MkdirAll(backupDir, 0o750); err != nil {
			log.Printf("duplicates: mkdir %s: %v", backupDir, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "backup dir unavailable"})
		}

		if strings.TrimSpace(req.Keep) == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "keep is required"})
		}
		keep, err := resolveLibraryPath(libraryRoot, req.Keep)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid keep path"})
		}

		moved, skipped := []string{}, []string{}
		for _, raw := range req.Paths {
			src, err := resolveLibraryPath(libraryRoot, raw)
			if err != nil || (keep != "" && src == keep) {
				skipped = append(skipped, raw)
				continue
			}

			target, err := reserveBackupTarget(backupDir, filepath.Base(src))
			if err != nil {
				log.Printf("duplicates: reserve target for %s: %v", src, err)
				skipped = append(skipped, raw)
				continue
			}

			if err := os.Rename(src, target); err != nil {
				_ = os.Remove(target)
				log.Printf("duplicates: rename %s -> %s: %v", src, target, err)
				skipped = append(skipped, raw)
				continue
			}
			moved = append(moved, raw)
		}

		return c.JSON(fiber.Map{"status": "ok", "moved": moved, "skipped": skipped})
	})

	return app
}

var errUnsafePath = errors.New("unsafe library path")

// resolveLibraryPath возвращает реальный путь ТОЛЬКО если p — обычный файл
// внутри libraryRoot (после Clean и EvalSymlinks).
func resolveLibraryPath(libraryRoot, p string) (string, error) {
	if strings.TrimSpace(p) == "" || !filepath.IsAbs(p) {
		return "", errUnsafePath
	}

	root, err := filepath.Abs(filepath.Clean(libraryRoot))
	if err != nil || root == string(filepath.Separator) {
		return "", fmt.Errorf("%w: invalid library root", errUnsafePath)
	}

	src, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", errUnsafePath
	}

	rel, err := filepath.Rel(root, src)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errUnsafePath
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("library root: %w", err)
	}
	realSrc, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", err
	}

	rel, err = filepath.Rel(realRoot, realSrc)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errUnsafePath
	}

	fi, err := os.Lstat(realSrc)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%w: not a regular file", errUnsafePath)
	}
	return realSrc, nil
}

// reserveBackupTarget атомарно резервирует имя в backupDir, чтобы два
// параллельных запроса не перетёрли один и тот же файл.
func reserveBackupTarget(dir, base string) (string, error) {
	if base == "" || base == "." || base == ".." ||
		strings.ContainsAny(base, `/\`) {
		return "", errUnsafePath
	}

	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d-%d%s", stem, time.Now().UnixNano(), i, ext)
		}
		target := filepath.Join(dir, name)

		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return target, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("cannot allocate backup name")
}

// main поднимает агент на адресах из HOST_AGENT_BIND.
//
// P0: раньше было жёстко app.Listen("0.0.0.0:9998") — слушали все интерфейсы,
// и любой в LAN дёргал /update/run, /cleanup/run, /duplicates/resolve.
// Теперь по умолчанию ТОЛЬКО loopback (127.0.0.1:9998). Контейнеры стека
// ходят на агента через host.containers.internal, который резолвится не в
// loopback, а в адрес хоста, поэтому дополнительный адрес задаётся в .env:
//
//	HOST_AGENT_BIND=127.0.0.1:9998,192.168.0.4:9998
//
// Сам host-exec дополнительно закрыт ADMIN_TOKEN (см. requireAdminToken),
// так что даже при 0.0.0.0 в bind опасные ручки недоступны без токена.
func main() {
	binds := agentBindAddrs()
	hasToken := adminToken() != ""

	if !hasToken {
		log.Println("⚠️ ADMIN_TOKEN не задан — все ручки кроме /health и /metrics будут отдавать 403")
	}

	for _, addr := range binds {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			log.Fatalf("host-agent: некорректный HOST_AGENT_BIND %q: %v", addr, err)
		}
		if !isLoopbackAddr(addr) && !hasToken {
			log.Fatalf("host-agent: отказ запуска на %s без ADMIN_TOKEN", addr)
		}
	}

	for _, extra := range binds[1:] {
		addr := extra
		go func() {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				log.Printf("⚠️ host-agent: не открыт слушатель %s: %v", addr, err)
				return
			}
			log.Printf("🖥️  host-agent дополнительный слушатель: %s", addr)
			if err := newApp().Listener(ln); err != nil {
				log.Printf("⚠️ host-agent слушатель %s остановлен: %v", addr, err)
			}
		}()
	}

	log.Printf("🖥️  host-agent на %s", binds[0])
	if err := newApp().Listen(binds[0]); err != nil {
		log.Fatalf("host-agent: %v", err)
	}
}

// isLoopbackAddr — true для 127.0.0.0/8, ::1 и localhost.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// stackEnvValue — platform.GetEnv с fallback'ом на REPO_DIR/.env.
// Нативные юниты (systemd/dinit) не наследуют содержимое .env стека, а
// ADMIN_TOKEN и HOST_AGENT_BIND хранятся именно там (паттерн gatewayToken
// в notify-service).
func stackEnvValue(key, def string) string {
	if v := strings.TrimSpace(platform.GetEnv(key, "")); v != "" {
		return v
	}
	repoDir := platform.GetEnv("REPO_DIR", "/home/konstantin/projects/nas-stack")
	data, err := os.ReadFile(filepath.Join(repoDir, ".env"))
	if err != nil {
		return def
	}
	if v, ok := parseDotenv(data)[key]; ok {
		return v
	}
	return def
}

// parseDotenv разбирает .env: export, кавычки, inline-комментарии, значения с '='.
// При дубликатах ключа побеждает последнее значение — как при source в shell.
func parseDotenv(data []byte) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out[k] = dotenvValue(v)
	}
	return out
}

func dotenvValue(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	switch v[0] {
	case '"', '\'':
		if i := strings.LastIndexByte(v[1:], v[0]); i >= 0 {
			return v[1 : i+1] // содержимое кавычек как есть, '#' разрешён
		}
		return strings.Trim(v, `"'`)
	default:
		if i := strings.Index(v, " #"); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
}

// adminToken — секрет админ-операций (тот же ADMIN_TOKEN, что на gateway
// и scheduler).
func adminToken() string {
	return stackEnvValue("ADMIN_TOKEN", "")
}

// agentBindAddrs — список адресов прослушивания из HOST_AGENT_BIND
// (через запятую). По умолчанию только loopback.
func agentBindAddrs() []string {
	raw := stackEnvValue("HOST_AGENT_BIND", "127.0.0.1:9998")

	seen := make(map[string]struct{})
	out := make([]string, 0, 2)
	for _, a := range strings.Split(raw, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		host, port, err := net.SplitHostPort(a)
		if err != nil || host == "" || port == "" {
			log.Printf("⚠️ HOST_AGENT_BIND: пропущен некорректный адрес %q: %v", a, err)
			continue
		}
		if _, err := strconv.Atoi(port); err != nil {
			log.Printf("⚠️ HOST_AGENT_BIND: пропущен некорректный порт %q", a)
			continue
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	if len(out) == 0 {
		return []string{"127.0.0.1:9998"}
	}
	return out
}

// adminTokenOK — сверяет X-Admin-Token или Authorization: Bearer с ADMIN_TOKEN
// постоянным временем сравнения. Пустой ADMIN_TOKEN = fail-closed.
func adminTokenOK(c fiber.Ctx) bool {
	expected := adminToken()
	if expected == "" {
		return false
	}

	candidates := []string{strings.TrimSpace(c.Get("X-Admin-Token"))}
	if auth := strings.TrimSpace(c.Get("Authorization")); len(auth) >= 7 && strings.EqualFold(auth[:7], "Bearer ") {
		candidates = append(candidates, strings.TrimSpace(auth[7:]))
	}

	// Хэш фиксированной длины: убирает утечку длины токена через
	// ConstantTimeCompare (при разной длине он возвращается сразу).
	want := sha256.Sum256([]byte(expected))
	for _, got := range candidates {
		if got == "" {
			continue
		}
		h := sha256.Sum256([]byte(got))
		if subtle.ConstantTimeCompare(h[:], want[:]) == 1 {
			return true
		}
	}
	return false
}

// requireAdminToken — миддлварь для ручек с побочными эффектами на хосте:
// /update/run (git+compose), /cleanup/run (podman prune, rm -rf trash),
// /duplicates/resolve (перемещение файлов библиотеки).
func requireAdminToken(c fiber.Ctx) error {
	if adminTokenOK(c) {
		return c.Next()
	}
	return c.Status(403).JSON(fiber.Map{"error": "forbidden: admin token required"})
}

func kernelVersion() string {
	out, _ := execOutput(3*time.Second, "uname", "-r")
	return strings.TrimSpace(string(out))
}

func atof(f []string, i int) float64 {
	if i < len(f) {
		v, _ := strconv.ParseFloat(f[i], 64)
		return v
	}
	return 0
}

// ============================== SENSORS ==============================

var (
	tempRe = regexp.MustCompile(`(?i)temp|package|composite|core|tctl|tccd|sensor`)
	coreRe = regexp.MustCompile(`(?i)\bcore\b|tccd`)
)

// parseSensors разбирает JSON от `sensors -j` и извлекает температуры.
func parseSensors(data []byte) fiber.Map {
	var root map[string]map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return fiber.Map{"error": "parse: " + err.Error()}
	}

	type temp struct {
		Label string  `json:"label"`
		Temp  float64 `json:"temp"`
		Max   float64 `json:"max,omitempty"`
		Crit  float64 `json:"crit,omitempty"`
	}

	cpuPackage := fiber.Map{}
	cores := []temp{}
	nvme := []temp{}
	other := []temp{}

	for chip, features := range root {
		chipLow := strings.ToLower(chip)
		for feat, raw := range features {
			if !tempRe.MatchString(feat) {
				continue
			}
			subs, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			var input, max, crit float64
			hasInput := false
			for sub, val := range subs {
				f, ok := toFloat(val)
				if !ok {
					continue
				}
				ls := strings.ToLower(sub)
				switch {
				case strings.HasSuffix(ls, "input"):
					input = f
					hasInput = true
				case strings.HasSuffix(ls, "max"), strings.HasSuffix(ls, "high"):
					if f > max {
						max = f
					}
				case strings.HasSuffix(ls, "crit"):
					crit = f
				}
			}
			if !hasInput {
				continue
			}
			t := temp{Label: feat, Temp: input, Max: max, Crit: crit}
			switch {
			case strings.Contains(chipLow, "nvme"):
				nvme = append(nvme, t)
			case strings.Contains(strings.ToLower(feat), "package"):
				if _, ok := cpuPackage["temp"]; !ok {
					cpuPackage = fiber.Map{"label": feat, "temp": input, "max": max}
				}
			case coreRe.MatchString(feat):
				cores = append(cores, t)
			default:
				other = append(other, t)
			}
		}
	}

	sort.SliceStable(cores, func(i, j int) bool { return cores[i].Label < cores[j].Label })
	return fiber.Map{
		"cpu_package": cpuPackage,
		"cores":       cores,
		"nvme":        nvme,
		"other":       other,
	}
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// ============================== SMART ==============================

func getSmart() []fiber.Map {
	candidates := []string{"/dev/nvme0n1", "/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"}
	result := []fiber.Map{}
	for _, dev := range candidates {
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		result = append(result, smartOne(dev))
	}
	return result
}

func smartOne(dev string) fiber.Map {
	// smartctl требует root — пробуем через passwordless sudo.
	out, err := execOutput(20*time.Second, "sudo", "-n", "smartctl", "-a", "-j", dev)
	if err != nil && len(out) == 0 {
		out, err = execOutput(15*time.Second, "smartctl", "-a", "-j", dev)
	}
	if len(out) == 0 {
		msg := "smartctl failed"
		if err != nil {
			msg = err.Error()
		}
		return fiber.Map{"device": dev, "error": msg}
	}
	var d map[string]interface{}
	if json.Unmarshal(out, &d) != nil {
		return fiber.Map{"device": dev, "error": "parse error"}
	}
	// Проверяем сообщения smartctl (напр. Permission denied).
	if sm, ok := d["smartctl"].(map[string]interface{}); ok {
		if msgs, ok := sm["messages"].([]interface{}); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]interface{}); ok {
					if sev, _ := mm["severity"].(string); sev == "error" {
						s, _ := mm["string"].(string)
						return fiber.Map{"device": dev, "error": s}
					}
				}
			}
		}
	}

	status := "UNKNOWN"
	if ss, ok := d["smart_status"].(map[string]interface{}); ok {
		if passed, ok := ss["passed"].(bool); ok {
			if passed {
				status = "PASSED"
			} else {
				status = "FAILED"
			}
		}
	}

	temp := 0
	if t, ok := d["temperature"].(map[string]interface{}); ok {
		temp = intAt(t, "current")
	}
	powerOn := 0
	if poh, ok := d["power_on_time"].(map[string]interface{}); ok {
		powerOn = intAt(poh, "hours")
	}

	wear := -1
	if nv, ok := d["nvme_smart_health_information_log"].(map[string]interface{}); ok {
		wear = intAt(nv, "percentage_used")
		if t := intAt(nv, "temperature"); t > 0 {
			temp = t
		}
		if h := intAt(nv, "power_on_hours"); h > 0 {
			powerOn = h
		}
	}

	model, _ := d["model_name"].(string)
	serial, _ := d["serial_number"].(string)
	proto, _ := d["device"].(map[string]interface{})
	protocol := ""
	if proto != nil {
		protocol, _ = proto["protocol"].(string)
	}

	res := fiber.Map{
		"device":         dev,
		"model":          model,
		"serial":         serial,
		"protocol":       protocol,
		"health":         status,
		"smart_status":   status == "PASSED",
		"temperature":    temp,
		"power_on_hours": powerOn,
	}
	if wear >= 0 {
		res["wear_level"] = wear
	}
	return res
}

func intAt(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	if v, ok := m[key]; ok {
		if f, ok := toFloat(v); ok {
			return int(f)
		}
	}
	return 0
}

// int64At — счётчики сети/байты не должны переполнять int на 32-битных платформах.
func int64At(m map[string]interface{}, key string) int64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key]; ok {
		if f, ok := toFloat(v); ok {
			return int64(f)
		}
	}
	return 0
}

// ============================== DISKS ==============================

var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "btrfs": true, "xfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "cifs": true, "nfs": true, "nfs4": true, "zfs": true,
}

func getDisks() []fiber.Map {
	out, _ := execOutput(10*time.Second, "df", "-h", "--output=source,fstype,size,used,avail,pcent,target")
	type disk struct{ source, fstype, total, used, avail, percent, mount string }
	seen := map[string]int{}
	list := []disk{}
	for _, line := range strings.Split(string(out), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 7 || !realFS[f[1]] {
			continue
		}
		d := disk{f[0], f[1], f[2], f[3], f[4], f[5], f[6]}
		if idx, ok := seen[f[0]]; ok {
			if len(d.mount) < len(list[idx].mount) {
				list[idx] = d
			}
			continue
		}
		seen[f[0]] = len(list)
		list = append(list, d)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].mount < list[j].mount })
	result := []fiber.Map{}
	for _, d := range list {
		result = append(result, fiber.Map{
			"source": d.source, "fstype": d.fstype, "total": d.total,
			"used": d.used, "avail": d.avail, "percent": d.percent, "mount": d.mount,
		})
	}
	return result
}

// ============================== PROCESSES ==============================

func getProcesses(limit int) []fiber.Map {
	out, _ := execOutput(5*time.Second, "ps", "aux", "--sort=-%cpu")
	result := []fiber.Map{}
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 11 {
			continue
		}
		name := strings.Join(f[10:], " ")
		if strings.HasPrefix(name, "ps ") {
			continue
		}
		result = append(result, fiber.Map{"user": f[0], "pid": f[1], "cpu": f[2], "mem": f[3], "name": name})
		if len(result) >= limit {
			break
		}
	}
	return result
}

// ============================== NETWORK ==============================

func getNetwork(iface string) fiber.Map {
	out, err := execOutput(5*time.Second, "ip", "-s", "-j", "link", "show", iface)
	if err != nil {
		return fiber.Map{"error": err.Error()}
	}
	var arr []map[string]interface{}
	if json.Unmarshal(out, &arr) != nil || len(arr) == 0 {
		return fiber.Map{"error": "parse error"}
	}
	obj := arr[0]
	stats, _ := obj["stats64"].(map[string]interface{})
	if stats == nil {
		stats, _ = obj["stats"].(map[string]interface{})
	}
	rx, _ := stats["rx"].(map[string]interface{})
	tx, _ := stats["tx"].(map[string]interface{})
	return fiber.Map{
		"iface":      iface,
		"rx_bytes":   int64At(rx, "bytes"),
		"tx_bytes":   int64At(tx, "bytes"),
		"rx_errors":  int64At(rx, "errors"),
		"tx_errors":  int64At(tx, "errors"),
		"rx_dropped": int64At(rx, "dropped"),
		"tx_dropped": int64At(tx, "dropped"),
		"rx_packets": int64At(rx, "packets"),
		"tx_packets": int64At(tx, "packets"),
	}
}

// ============================== CLEANUP ==============================

func cleanupPreview() []fiber.Map {
	items := []fiber.Map{}

	// podman
	if s := podmanReclaimable(); s > 0 {
		items = append(items, fiber.Map{"name": "podman_images", "size_bytes": s, "description": "Образы/контейнеры podman (prune)"})
	}
	// journald
	items = append(items, fiber.Map{"name": "journald", "size_bytes": dirSizeCmd("journalctl", "--disk-usage"), "description": "journald logs"})
	// cache
	home, _ := os.UserHomeDir()
	items = append(items, fiber.Map{"name": "cache", "size_bytes": duSize(filepathJoin(home, ".cache")), "description": "~/.cache"})
	// trash
	trash := duSize(filepathJoin(home, ".local/share/Trash"))
	items = append(items, fiber.Map{"name": "trash", "size_bytes": trash, "description": "Корзина"})
	// pacman
	items = append(items, fiber.Map{"name": "pacman_cache", "size_bytes": duSize("/var/cache/pacman/pkg"), "description": "pacman cache"})
	// old kernels (количество)
	items = append(items, fiber.Map{"name": "old_kernels", "size_bytes": 0, "count": oldKernels(), "description": "Старые ядра (не удаляются автоматически)"})

	return items
}

var cleanupTargets = map[string]bool{
	"podman_images": true,
	"journald":      true,
	"cache":         true,
	"trash":         true,
	"pacman_cache":  true,
}

// cleanupMu не даёт двум очисткам одновременно считать before/after.
var cleanupMu sync.Mutex

func runCleanup(targets []string, all bool) fiber.Map {
	want := make(map[string]bool, len(targets))
	if all {
		for t := range cleanupTargets {
			want[t] = true
		}
	} else {
		for _, t := range targets {
			if cleanupTargets[t] {
				want[t] = true
			}
		}
	}

	results := []fiber.Map{}
	freed := int64(0)
	home, homeErr := os.UserHomeDir()

	apply := func(name string, fn func(context.Context) error) {
		if !want[name] {
			return
		}
		before := cleanupSizeFor(name)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		if err := fn(ctx); err != nil {
			log.Printf("cleanup %s: %v", name, err)
			results = append(results, fiber.Map{"target": name, "ok": false, "error": "cleanup failed"})
			return
		}

		after := cleanupSizeFor(name)
		f := before - after
		if f < 0 {
			f = 0
		}
		freed += f
		results = append(results, fiber.Map{"target": name, "ok": true, "freed_bytes": f})
	}

	// Никакого sh -c: только argv, без ~ и без интерполяции.
	apply("podman_images", func(ctx context.Context) error {
		return exec.CommandContext(ctx, "podman", "system", "prune", "-a", "--volumes", "-f").Run()
	})
	apply("journald", func(ctx context.Context) error {
		return exec.CommandContext(ctx, "journalctl", "--vacuum-size=500M").Run()
	})
	apply("cache", func(ctx context.Context) error {
		if homeErr != nil {
			return homeErr
		}
		dir := filepath.Join(home, ".cache")
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return exec.CommandContext(ctx, "find", dir, "-type", "f", "-atime", "+30", "-delete").Run()
	})
	apply("trash", func(ctx context.Context) error {
		if homeErr != nil {
			return homeErr
		}
		return emptyDir(filepath.Join(home, ".local", "share", "Trash"))
	})
	apply("pacman_cache", func(ctx context.Context) error {
		return exec.CommandContext(ctx, "paccache", "-rk3").Run()
	})

	return fiber.Map{"dry": false, "freed_bytes": freed, "results": results}
}

// emptyDir удаляет содержимое каталога, но не сам каталог (точку монтирования).
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func podmanReclaimable() int64 {
	out, err := execOutput(20*time.Second, "podman", "system", "df", "--format", "json")
	if err != nil || len(out) == 0 {
		return 0
	}
	var arr []map[string]interface{}
	if json.Unmarshal(out, &arr) != nil {
		return 0
	}
	var total int64
	for _, item := range arr {
		if v, ok := item["RawReclaimable"].(float64); ok {
			total += int64(v)
		}
	}
	return total
}

func cleanupSizeFor(name string) int64 {
	home, _ := os.UserHomeDir()
	switch name {
	case "podman_images":
		return podmanReclaimable()
	case "journald":
		return dirSizeCmd("journalctl", "--disk-usage")
	case "cache":
		return duSize(filepathJoin(home, ".cache"))
	case "trash":
		return duSize(filepathJoin(home, ".local/share/Trash"))
	case "pacman_cache":
		return duSize("/var/cache/pacman/pkg")
	}
	return 0
}

// dirSizeCmd — размер из команды вида "journalctl --disk-usage" (парсим "T archived").
func dirSizeCmd(name string, args ...string) int64 {
	out, err := execOutput(10*time.Second, name, args...)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	// пример: "Archived and active journals take up 123.4M in the file system."
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	return parseHumanSize(m[1] + m[2])
}

var sizeRe = regexp.MustCompile(`([0-9]+(?:[.,][0-9]+)?)\s*([KMGTP]?i?B?)`)

func duSize(path string) int64 {
	out, _ := execOutput(30*time.Second, "du", "-sb", path)
	f := strings.Fields(string(out))
	if len(f) < 1 {
		return 0
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	return n
}

func oldKernels() int {
	cur := kernelVersion()
	entries, err := os.ReadDir("/usr/lib/modules")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.Name() != cur {
			n++
		}
	}
	return n
}

// parseHumanSize переводит "1.5G"/"300M"/"1.2GB"/"1 GiB" в байты.
// Мусор, NaN/Inf и отрицательные значения → 0; переполнение клампится в MaxInt64.
func parseHumanSize(s string) int64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0
	}
	if last := s[len(s)-1]; last == 'B' || last == 'b' {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	// "1GiB" -> "1G"
	if s != "" {
		if last := s[len(s)-1]; last == 'i' || last == 'I' {
			s = strings.TrimSpace(s[:len(s)-1])
		}
	}

	mult := float64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'K', 'k':
			mult = 1 << 10
		case 'M', 'm':
			mult = 1 << 20
		case 'G', 'g':
			mult = 1 << 30
		case 'T', 't':
			mult = 1 << 40
		case 'P', 'p':
			mult = 1 << 50
		}
		if mult != 1 {
			s = strings.TrimSpace(s[:len(s)-1])
		}
	}

	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	// float→int64 вне диапазона даёт MinInt64 на amd64 — клампим заранее.
	if f >= float64(math.MaxInt64)/mult {
		return math.MaxInt64
	}
	return int64(f * mult)
}

func filepathJoin(a, b string) string { return filepath.Join(a, b) }
func filepathBase(p string) string    { return filepath.Base(p) }
func filepathExt(p string) string     { return filepath.Ext(p) }

var secretLineRe = regexp.MustCompile(`(?i)((?:token|password|secret|key)\s*=\s*)[^\s]+`)

// redactLog убирает секреты из вывода git/compose и ограничивает размер записи в journald.
func redactLog(b []byte) string {
	s := secretLineRe.ReplaceAllString(string(b), `${1}<redacted>`)
	const max = 4096
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
