package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"
)

const (
	defaultRepoDir = "/home/konstantin/projects/nas-stack"
	maxNotifyRunes = 2000
	jobTimeout     = 20 * time.Minute
)

var (
	envKeyRe     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	serviceRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	ntfyTopicRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tokenValueRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,128}$`)

	secretQueryRe = regexp.MustCompile(`(?i)(passkey|token|apikey|api_key|password|secret)=([^&\s]+)`)

	// protectedEnvKeys — переменные, через которые .env влияет на запуск
	// compose/шелла: запись через UI = обход изоляции.
	protectedEnvKeys = map[string]struct{}{
		"PATH": {}, "LD_PRELOAD": {}, "LD_LIBRARY_PATH": {}, "BASH_ENV": {},
		"ENV": {}, "GODEBUG": {}, "COMPOSE_FILE": {}, "COMPOSE_PROJECT_NAME": {},
		"COMPOSE_PROFILES": {}, "DOCKER_HOST": {}, "CONTAINER_HOST": {},
	}

	envMu sync.Mutex

	jobBusy atomic.Bool
	jobLog  atomic.Value // string

	// notify-send — ограниченный пул: флуд /send не должен форкать процессы без лимита.
	notifySem = make(chan struct{}, 4)

	ntfyClient = &http.Client{
		Timeout: 10 * time.Second,
		// NTFY_URL не должен превращаться в SSRF-прыжок через редирект.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rdb := redis.NewClient(&redis.Options{
		Addr:     platform.GetEnv("REDIS_ADDR", "localhost:6379"),
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	defer func() { _ = rdb.Close() }()

	app := fiber.New(fiber.Config{
		BodyLimit:    64 << 10,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
		ProxyHeader:  "", // c.IP() не должен читать X-Forwarded-For
	})
	app.Use(func(c fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		return c.Next()
	})
	registerRoutes(app, rdb)

	serverErr := make(chan error, 1)
	go func() {
		// ВАЖНО: слушаем все интерфейсы намеренно. Контейнеры стека ходят сюда
		// через host.containers.internal:8083 (см. NOTIFY_SERVICE_URL в
		// docker-compose.yml), поэтому 127.0.0.1 сломал бы доставку уведомлений.
		// Не менять на loopback без перевода потребителей на unix-сокет/сеть.
		addr := platform.GetEnv("NOTIFY_BIND", "0.0.0.0:8083")
		log.Printf("🌐 notify-service HTTP на %s", addr)
		serverErr <- app.Listen(addr)
	}()
	go func() {
		<-ctx.Done()
		_ = app.ShutdownWithTimeout(10 * time.Second)
	}()

	if gatewayToken() == "" {
		log.Println("⚠️ NOTIFY_GATEWAY_TOKEN не задан — /update/*, /env POST и /env/reload будут отдавать 403")
	}

	pubsub := rdb.Subscribe(ctx,
		"torrent.added",
		"torrent.completed",
		"disk.warning",
		"disk.critical",
	)
	defer func() { _ = pubsub.Close() }()

	// Ждём подтверждения подписки: иначе NOAUTH/ACL выглядят как «тишина».
	if err := pubsub.Ping(ctx); err != nil {
		log.Fatalf("redis subscribe: %v", err)
	}
	log.Println("👀 notify-service слушает Redis...")

	ch := pubsub.Channel()
	sem := make(chan struct{}, 8)
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-serverErr:
			// При штатном shutdown Listen тоже возвращает ошибку — не падаем.
			if err != nil && ctx.Err() == nil {
				log.Fatalf("notify http: %v", err)
			}
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// Redis-канал недоверенный: режем длину и секреты в query (magnet ?passkey=).
			log.Printf("📥 [%s] %s", msg.Channel, truncate(redact(msg.Payload), 200))
			select {
			case sem <- struct{}{}:
				go func(m *redis.Message) {
					defer func() { <-sem }()
					sendNotification(m.Channel, m.Payload)
				}(msg)
			default:
				log.Printf("⚠️ очередь уведомлений переполнена, пропущено [%s]", msg.Channel)
			}
		}
	}
}

func registerRoutes(app *fiber.App, rdb *redis.Client) {
	app.Get("/health", func(c fiber.Ctx) error {
		cctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
		defer cancel()
		if rdb != nil {
			if err := rdb.Ping(cctx).Err(); err != nil {
				return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
					"status": "degraded", "redis": "unavailable",
				})
			}
		}
		return c.JSON(fiber.Map{"status": "ok", "service": "notify"})
	})

	app.Post("/send", requireLocalOrToken, handleSend)

	app.Post("/update/run", requireGatewayToken, handleUpdateRun)
	app.Post("/update/rollback", requireGatewayToken, handleRollback)
	app.Get("/update/status", requireGatewayToken, handleUpdateStatus)

	app.Get("/env", requireLocalOrToken, handleEnvGet)
	app.Post("/env", requireGatewayToken, handleEnvSet)
	app.Post("/env/reload", requireGatewayToken, handleEnvReload)

	app.Get("/oom-check", requireLocalOrToken, handleOOMCheck)
}

// ---------- /send ----------

func handleSend(c fiber.Ctx) error {
	var req struct {
		Text     string `json:"text"`
		Severity string `json:"severity"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad json"})
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len([]rune(text)) > maxNotifyRunes {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "text required (max 2000 chars)"})
	}

	sev := normalizeSeverity(req.Severity)
	icon, urgency := severityStyle(sev)

	desktopErr := notifyDesktop("NAS", text, icon, urgency)
	ntfyErr := sendNtfy(c.Context(), "NAS", text, sev, icon)
	if desktopErr != nil {
		log.Printf("send: desktop: %v", desktopErr)
	}
	if ntfyErr != nil {
		log.Printf("send: ntfy: %v", ntfyErr)
	}
	if desktopErr != nil && ntfyErr != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "delivery failed"})
	}
	return c.JSON(fiber.Map{"status": "sent"})
}

// ---------- /update ----------

func handleUpdateRun(c fiber.Ctx) error {
	if !startJob("update", runUpdateScript) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "job already running"})
	}
	return c.JSON(fiber.Map{"status": "started"})
}

func handleRollback(c fiber.Ctx) error {
	if !startJob("rollback", runRollback) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "job already running"})
	}
	return c.JSON(fiber.Map{"status": "rollback_started"})
}

func handleUpdateStatus(c fiber.Ctx) error {
	out, _ := jobLog.Load().(string)
	return c.JSON(fiber.Map{"running": jobBusy.Load(), "log": redact(out)})
}

// startJob — single-flight: второй update/rollback/reload получает 409,
// а не конкурирующий git reset/podman build по одному worktree.
func startJob(name string, fn func(context.Context) ([]byte, error)) bool {
	if !jobBusy.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer jobBusy.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
		defer cancel()
		out, err := fn(ctx)
		jobLog.Store(fmt.Sprintf("%s err=%v\n%s", name, err, tail(out, 8<<10)))
		log.Printf("job %s: err=%v", name, err)
	}()
	return true
}

func runUpdateScript(ctx context.Context) ([]byte, error) {
	dir, err := repoDirChecked()
	if err != nil {
		return nil, err
	}
	script := filepath.Join(dir, "scripts", "update-stack.sh")
	if st, err := os.Stat(script); err != nil || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("update script not executable: %s", script)
	}
	cmd := exec.CommandContext(ctx, "bash", "--", script)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func runRollback(ctx context.Context) ([]byte, error) {
	dir, err := repoDirChecked()
	if err != nil {
		return nil, err
	}
	// git reset --hard молча уничтожает локальные правки — отказываем.
	if status, err := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain").Output(); err == nil {
		if strings.TrimSpace(string(status)) != "" {
			return nil, errors.New("в репозитории есть незакоммиченные изменения — rollback отменён")
		}
	}

	var out []byte
	step := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		b, err := cmd.CombinedOutput()
		out = append(out, b...)
		return err
	}
	if err := step("git", "reset", "--hard", "HEAD~1"); err != nil {
		return out, err
	}
	if err := step("podman", "compose", "build"); err != nil {
		return out, err
	}
	return out, step("podman", "compose", "up", "-d")
}

// ---------- /env ----------

func handleEnvGet(c fiber.Ctx) error {
	envMu.Lock()
	data, err := os.ReadFile(envPath())
	envMu.Unlock()
	if err != nil {
		log.Printf("env read: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "env not readable"})
	}

	vars := make([]fiber.Map, 0, 32)
	for _, kv := range parseEnv(string(data)) {
		// Ключи из руками правленного .env не проходили POST /env.
		// Дашборд подставляет их в HTML/JS-строки — отдаём только ключи,
		// которые API сам разрешает записывать.
		if !envKeyRe.MatchString(kv.Key) {
			continue
		}
		val := kv.Value
		if isSecret(kv.Key) || looksLikeCredentialURL(val) {
			val = "***"
		}
		vars = append(vars, fiber.Map{"key": kv.Key, "value": val})
	}
	return c.JSON(fiber.Map{"vars": vars})
}

func handleEnvSet(c fiber.Ctx) error {
	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad json"})
	}
	if !envKeyRe.MatchString(req.Key) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid key"})
	}
	if _, blocked := protectedEnvKeys[req.Key]; blocked {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "key is not writable"})
	}
	if !validEnvValue(req.Value) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid value"})
	}
	// Самоблокировка: битый токен отрезает доступ к /env и /update навсегда.
	if req.Key == "NOTIFY_GATEWAY_TOKEN" && !tokenValueRe.MatchString(req.Value) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid NOTIFY_GATEWAY_TOKEN"})
	}

	envMu.Lock()
	defer envMu.Unlock()
	if err := setEnvValue(envPath(), req.Key, req.Value); err != nil {
		log.Printf("env write %s: %v", req.Key, err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "cannot save .env"})
	}
	return c.JSON(fiber.Map{"status": "saved"})
}

func handleEnvReload(c fiber.Ctx) error {
	var req struct {
		Service string `json:"service"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad json"})
	}
	if !serviceRe.MatchString(req.Service) || !reloadableService(req.Service) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid service"})
	}
	dir, err := repoDirChecked()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "NAS_STACK_DIR is not available"})
	}

	svc := req.Service
	if !startJob("reload:"+svc, func(ctx context.Context) ([]byte, error) {
		// serviceRe запрещает ведущий "-": инъекция опций в podman compose невозможна.
		cmd := exec.CommandContext(ctx, "podman", "compose", "up", "-d", "--force-recreate", svc)
		cmd.Dir = dir
		return cmd.CombinedOutput()
	}) {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "job already running"})
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"status": "reloading"})
}

func reloadableService(s string) bool {
	switch s {
	case "gateway", "notify", "redis", "postgres", "db":
		return false
	}
	return true
}

// ---------- /oom-check ----------

func handleOOMCheck(c fiber.Ctx) error {
	n, err := strconv.Atoi(strings.TrimSpace(platform.GetEnv("OOM_CHECK_MINUTES", "20")))
	if err != nil || n <= 0 || n > 24*60 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid OOM_CHECK_MINUTES"})
	}

	ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
	defer cancel()
	// Никакого sh -c: значение --since формируется из проверенного int.
	out, err := exec.CommandContext(ctx, "journalctl", "-k", "--no-pager", "-o", "cat",
		"--since", fmt.Sprintf("%d min ago", n)).Output()
	if err != nil {
		log.Printf("oom-check: %v", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "journalctl failed"})
	}

	var hits []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(strings.ToLower(l), "killed process") {
			hits = append(hits, l)
		}
	}
	if len(hits) > 5 {
		hits = hits[len(hits)-5:]
	}
	return c.JSON(fiber.Map{"lines": strings.Join(hits, "\n")})
}

// ---------- уведомления ----------

func sendNotification(channel, payload string) {
	var title, sev, icon, urgency string
	switch channel {
	case "torrent.added":
		title, sev, icon, urgency = "📥 Торрент добавлен", "info", "download", "normal"
		payload = truncate(payload, 100)
	case "torrent.completed":
		title, sev, icon, urgency = "✅ Торрент завершён", "info", "dialog-information", "normal"
		payload = truncate(payload, 100)
	case "disk.warning":
		title, sev, icon, urgency = "⚠️ Диск заполнен", "warning", "dialog-warning", "normal"
	case "disk.critical":
		title, sev, icon, urgency = "🔴 Диск переполнен", "critical", "dialog-error", "critical"
	default:
		title, sev, icon, urgency = "Уведомление", "info", "dialog-information", "normal"
	}
	body := truncate(strings.ToValidUTF8(payload, "?"), 500)

	if err := notifyDesktop(title, body, icon, urgency); err != nil {
		log.Printf("❌ %v", err)
	}
	if err := sendNtfy(context.Background(), title, body, sev, icon); err != nil {
		log.Printf("❌ ntfy: %v", err)
	}
}

// notifyDesktop — единственная точка вызова notify-send: таймаут, лимит,
// UTF-8 и "--" перед данными (иначе текст вида "--wait" станет опцией).
func notifyDesktop(title, body, icon, urgency string) error {
	select {
	case notifySem <- struct{}{}:
		defer func() { <-notifySem }()
	default:
		return errors.New("notification queue full")
	}

	title = sanitizeHeader(title, 120)
	body = truncate(strings.ToValidUTF8(body, "?"), 500)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "notify-send",
		"--app-name=NAS", "--icon="+icon, "--urgency="+urgency,
		"--", title, body)
	cmd.Env = append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("notify-send: %w (%s)", err, truncate(string(out), 200))
	}
	return nil
}

func sendNtfy(ctx context.Context, title, body, severity, icon string) error {
	raw := strings.TrimSpace(os.Getenv("NTFY_URL"))
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("ntfy: invalid NTFY_URL")
	}

	topic := platform.GetEnv("NTFY_TOPIC", "nas-alerts")
	if !ntfyTopicRe.MatchString(topic) {
		return errors.New("ntfy: invalid NTFY_TOPIC")
	}
	u.Path = path.Join(u.Path, topic) // без конкатенации: "../", "?", "#" невозможны
	u.RawQuery, u.Fragment = "", ""

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", sanitizeHeader(title, 200))
	req.Header.Set("Priority", priorityFor(severity))
	req.Header.Set("Tags", sanitizeHeader(icon, 64))
	if token := strings.TrimSpace(os.Getenv("NTFY_TOKEN")); token != "" {
		if u.Scheme == "http" {
			log.Println("⚠️ ntfy: NTFY_TOKEN отправляется по http")
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := ntfyClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}

func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "error", "warning", "info":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return "info"
	}
}

func severityStyle(sev string) (icon, urgency string) {
	switch sev {
	case "critical", "error":
		return "dialog-error", "critical"
	case "warning":
		return "dialog-warning", "normal"
	default:
		return "dialog-information", "normal"
	}
}

func priorityFor(sev string) string {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical", "error":
		return "5"
	case "warning":
		return "4"
	case "info":
		return "2"
	default:
		return "3"
	}
}

func sanitizeHeader(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	return truncate(s, max)
}

// ---------- .env ----------

type envVar struct{ Key, Value string }

// parseEnv — единый парсер для GET /env, POST /env и gatewayToken.
// Понимает export/пробелы/кавычки/inline-комментарии; иначе один и тот же
// файл читался тремя разными способами и ключи дублировались.
func parseEnv(content string) []envVar {
	var out []envVar
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		v = strings.TrimSpace(v)
		quoted := false
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
			quoted = true
		}
		if !quoted {
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		}
		out = append(out, envVar{Key: k, Value: v})
	}
	return out
}

func validEnvValue(v string) bool {
	if len(v) > 4096 || v == "***" || v != strings.TrimSpace(v) {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
		switch r {
		case '"', '\'', '`', '\\', '$', '#':
			// ломает кавычки/комментарии/интерполяцию compose
			return false
		}
	}
	return true
}

func setEnvValue(p, key, value string) error {
	orig, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Не затираем .env одной строкой при EACCES/IO-ошибке.
		return fmt.Errorf("read .env: %w", err)
	}

	var lines []string
	if len(orig) > 0 {
		lines = strings.Split(strings.TrimRight(string(orig), "\n"), "\n")
	}

	out := make([]string, 0, len(lines)+1)
	replaced := false
	for _, line := range lines {
		k, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if ok && strings.TrimSpace(k) == key {
			if replaced {
				continue // дубль ключа: compose взял бы последний
			}
			out = append(out, key+"="+value)
			replaced = true
			continue
		}
		out = append(out, line)
	}
	if !replaced {
		out = append(out, key+"="+value)
	}

	if len(orig) > 0 {
		if err := writeFileAtomic(p+".bak", orig, 0o600); err != nil {
			return fmt.Errorf("backup .env: %w", err)
		}
	}
	// Всегда 0600: раньше .env/.bak оставались 0644 (world-readable).
	return writeFileAtomic(p, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

func writeFileAtomic(p string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".env.tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, p) // атомарная подмена: читатели не видят полузаписанный файл
}

func repoDir() string { return platform.GetEnv("NAS_STACK_DIR", defaultRepoDir) }

func envPath() string { return filepath.Join(repoDir(), ".env") }

func repoDirChecked() (string, error) {
	abs, err := filepath.Abs(repoDir())
	if err != nil || !filepath.IsAbs(abs) {
		return "", errors.New("bad NAS_STACK_DIR")
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", errors.New("NAS_STACK_DIR is not a directory")
	}
	return abs, nil
}

func tail(b []byte, max int) string {
	if max <= 0 || len(b) == 0 {
		return ""
	}
	if len(b) <= max {
		return string(b)
	}
	return "…" + string(b[len(b)-max:])
}

func redact(s string) string {
	return secretQueryRe.ReplaceAllString(s, "$1=***")
}

// ---------- auth ----------

func isLocal(c fiber.Ctx) bool { return isLocalIP(c.IP()) }

func isLocalIP(ip string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	return a.Unmap().IsLoopback() // ::ffff:127.0.0.1 тоже loopback
}

// loopbackHost — защита от DNS rebinding: браузер с loopback-адресом
// не должен приниматься с Host: evil.com.
func loopbackHost(hostport string) bool {
	h := strings.TrimSpace(hostport)
	if h == "" {
		return false
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.Unmap().IsLoopback()
}

func loopbackOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && loopbackHost(u.Host)
}

func gatewayToken() string {
	if t := strings.TrimSpace(platform.GetEnv("NOTIFY_GATEWAY_TOKEN", "")); t != "" {
		return t
	}
	data, err := os.ReadFile(envPath())
	if err != nil {
		return ""
	}
	var token string
	for _, kv := range parseEnv(string(data)) {
		if kv.Key == "NOTIFY_GATEWAY_TOKEN" {
			token = kv.Value // как docker compose: побеждает последний
		}
	}
	return token
}

// bearerToken — регистронезависимая схема Bearer (RFC 7235).
func bearerToken(h string) string {
	scheme, val, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(val)
}

// tokensEqual сравнивает хэши: ConstantTimeCompare на разных длинах выходит сразу.
func tokensEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

func gatewayTokenOK(c fiber.Ctx) bool {
	expected := gatewayToken()
	if expected == "" {
		return false // fail-closed
	}
	got := c.Get("X-Gateway-Token")
	if got == "" {
		got = bearerToken(c.Get("Authorization"))
	}
	if got == "" {
		return false
	}
	return tokensEqual(got, expected)
}

func requireGatewayToken(c fiber.Ctx) error {
	if gatewayTokenOK(c) {
		return c.Next()
	}
	return c.Status(403).JSON(fiber.Map{"error": "forbidden: gateway token required"})
}

func requireLocalOrToken(c fiber.Ctx) error {
	if gatewayTokenOK(c) {
		return c.Next()
	}
	if isLocal(c) && loopbackHost(c.Host()) && loopbackOrigin(c.Get("Origin")) {
		return c.Next()
	}
	return c.Status(403).JSON(fiber.Map{"error": "forbidden"})
}

// secretSegments — сравнение по сегментам ключа, а не по подстроке:
// COMPASS_DIR/MONKEY больше не маскируются, а AWS_ACCESS_KEY_ID и
// ssh_private_key — маскируются.
var secretSegments = map[string]struct{}{
	"PASS": {}, "PASSWD": {}, "PASSWORD": {}, "PWD": {},
	"SECRET": {}, "SECRETS": {}, "TOKEN": {}, "COOKIE": {}, "COOKIES": {},
	"CREDENTIAL": {}, "CREDENTIALS": {}, "SALT": {}, "PRIVATE": {},
	"APIKEY": {}, "KEY": {}, "DSN": {},
}

// isSecret — маскирует значения «секретных» переменных.
func isSecret(key string) bool {
	for _, seg := range strings.FieldsFunc(strings.ToUpper(strings.TrimSpace(key)), func(r rune) bool {
		return r < 'A' || r > 'Z'
	}) {
		if _, ok := secretSegments[seg]; ok {
			return true
		}
	}
	return false
}

// user:pass@host в URL — тоже секрет, даже если ключ безобидный.
func looksLikeCredentialURL(v string) bool {
	if !strings.Contains(v, "://") {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.User != nil
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
