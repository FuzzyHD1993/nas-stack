package main

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/redis/go-redis/v9"
)

func main() {
	rdb := redis.NewClient(&redis.Options{
		Addr: platform.GetEnv("REDIS_ADDR", "localhost:6379"),
	})

	// HTTP-сервер для health check
	go func() {
		app := fiber.New()
		app.Get("/health", func(c fiber.Ctx) error {
			return c.JSON(fiber.Map{"status": "ok", "service": "notify"})
		})
		app.Post("/send", requireLocalOrToken, func(c fiber.Ctx) error {
			var req struct {
				Text     string `json:"text"`
				Severity string `json:"severity"`
			}
			if err := c.Bind().JSON(&req); err != nil {
				return c.Status(400).JSON(fiber.Map{"error": "bad json"})
			}
			urgency := "normal"
			icon := "dialog-information"
			if req.Severity == "error" || req.Severity == "critical" {
				urgency = "critical"
				icon = "dialog-error"
			} else if req.Severity == "warning" {
				urgency = "normal"
				icon = "dialog-warning"
			}
			cmd := exec.Command("notify-send", "NAS", req.Text, "--icon="+icon, "--urgency="+urgency)
			if err := cmd.Run(); err != nil {
				log.Printf("❌ notify-send failed: %v", err)
			}
			sendNtfy("NAS", req.Text, req.Severity, icon)
			return c.JSON(fiber.Map{"status": "sent"})
		})

		// ---- Обновление стека (выполняется на хосте) ----
		repoDir := platform.GetEnv("NAS_STACK_DIR", "/home/konstantin/projects/nas-stack")

		app.Post("/update/run", requireGatewayToken, func(c fiber.Ctx) error {
			go func() {
				cmd := exec.Command("bash", repoDir+"/scripts/update-stack.sh")
				cmd.Dir = repoDir
				out, err := cmd.CombinedOutput()
				log.Printf("🔄 update-stack: err=%v\n%s", err, out)
			}()
			return c.JSON(fiber.Map{"status": "started"})
		})

		app.Post("/update/rollback", requireGatewayToken, func(c fiber.Ctx) error {
			go func() {
				cmd := exec.Command("bash", "-c",
					"cd "+repoDir+" && git reset --hard HEAD~1 && podman compose build && podman compose up -d")
				out, err := cmd.CombinedOutput()
				log.Printf("↩️ rollback: err=%v\n%s", err, out)
			}()
			return c.JSON(fiber.Map{"status": "rollback_started"})
		})

		// ---- Редактор .env (только с локальных адресов) ----
		envPath := repoDir + "/.env"
		app.Get("/env", requireLocalOrToken, func(c fiber.Ctx) error {
			data, err := os.ReadFile(envPath)
			if err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
			vars := []fiber.Map{}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) != 2 {
					continue
				}
				key, val := parts[0], parts[1]
				if isSecret(key) {
					val = "***"
				}
				vars = append(vars, fiber.Map{"key": key, "value": val})
			}
			return c.JSON(fiber.Map{"vars": vars})
		})

		app.Post("/env", requireGatewayToken, func(c fiber.Ctx) error {
			var req struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			}
			if err := c.Bind().JSON(&req); err != nil {
				return c.Status(400).JSON(fiber.Map{"error": "bad json"})
			}
			if !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(req.Key) {
				return c.Status(400).JSON(fiber.Map{"error": "invalid key"})
			}
			if strings.ContainsAny(req.Value, "\n\r") {
				return c.Status(400).JSON(fiber.Map{"error": "value must be single-line"})
			}
			data, _ := os.ReadFile(envPath)
			lines := strings.Split(string(data), "\n")
			found := false
			for i, line := range lines {
				if strings.HasPrefix(line, req.Key+"=") {
					lines[i] = req.Key + "=" + req.Value
					found = true
					break
				}
			}
			if !found {
				lines = append(lines, req.Key+"="+req.Value)
			}
			_ = os.WriteFile(envPath+".bak", data, 0o644)
			if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
			return c.JSON(fiber.Map{"status": "saved"})
		})

		// ---- OOM-детектор (dmesg_restrict=1 — читаем kernel-лог через journalctl) ----
		app.Get("/oom-check", requireLocalOrToken, func(c fiber.Ctx) error {
			minutes := platform.GetEnv("OOM_CHECK_MINUTES", "20")
			cmd := exec.Command("sh", "-c",
				"journalctl -k --since \""+minutes+" min ago\" 2>/dev/null | grep -i 'killed process' | tail -5")
			out, _ := cmd.CombinedOutput()
			return c.JSON(fiber.Map{"lines": strings.TrimSpace(string(out))})
		})

		app.Post("/env/reload", requireGatewayToken, func(c fiber.Ctx) error {
			var req struct {
				Service string `json:"service"`
			}
			_ = c.Bind().JSON(&req)
			if req.Service == "" {
				return c.Status(400).JSON(fiber.Map{"error": "service required"})
			}
			if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(req.Service) {
				return c.Status(400).JSON(fiber.Map{"error": "invalid service"})
			}
			cmd := exec.Command("podman", "compose", "up", "-d", "--force-recreate", req.Service)
			cmd.Dir = repoDir
			if out, err := cmd.CombinedOutput(); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error(), "output": string(out)})
			}
			return c.JSON(fiber.Map{"status": "reloaded"})
		})

		if gatewayToken() == "" {
			log.Println("⚠️ NOTIFY_GATEWAY_TOKEN не задан — /update/*, /env POST и /env/reload будут отдавать 403")
		}
		log.Println("🌐 notify-service HTTP на :8083")
		if err := app.Listen(":8083"); err != nil {
			log.Printf("⚠️ HTTP server failed: %v", err)
		}
	}()

	// Redis listener
	ctx := context.Background()
	pubsub := rdb.Subscribe(ctx,
		"torrent.added",
		"torrent.completed",
		"disk.warning",
		"disk.critical",
	)
	defer pubsub.Close()
	ch := pubsub.Channel()

	log.Println("👀 notify-service слушает Redis...")

	for msg := range ch {
		log.Printf("📥 [%s] %s", msg.Channel, msg.Payload)
		sendNotification(msg.Channel, msg.Payload)
	}
}

func sendNotification(channel, payload string) {
	var title, body, icon, urgency string
	urgency = "normal"

	switch channel {
	case "torrent.added":
		title = "📥 Торрент добавлен"
		body = truncate(payload, 100)
		icon = "download"
	case "torrent.completed":
		title = "✅ Торрент завершён"
		body = truncate(payload, 100)
		icon = "dialog-information"
	case "disk.warning":
		title = "⚠️ Диск заполнен"
		body = payload
		icon = "dialog-warning"
		urgency = "normal"
	case "disk.critical":
		title = "🔴 Диск переполнен"
		body = payload
		icon = "dialog-error"
		urgency = "critical"
	default:
		title = "Уведомление"
		body = payload
		icon = "dialog-information"
	}

	// Гарантируем корректный UTF-8 (иначе notify-send покажет кракозябры).
	title = strings.ToValidUTF8(title, "?")
	body = strings.ToValidUTF8(body, "?")

	cmd := exec.Command("notify-send", title, body, "--icon="+icon, "--urgency="+urgency)
	cmd.Env = append(os.Environ(), "LANG=C.UTF-8", "LC_ALL=C.UTF-8")
	if err := cmd.Run(); err != nil {
		log.Printf("❌ notify-send failed: %v", err)
	}

	sendNtfy(title, body, urgency, icon)
}

// sendNtfy отправляет уведомление в ntfy (если NTFY_URL задан).
func sendNtfy(title, body, urgency, icon string) {
	base := os.Getenv("NTFY_URL")
	if base == "" {
		return
	}
	topic := platform.GetEnv("NTFY_TOPIC", "nas-alerts")
	priority := "3"
	switch urgency {
	case "critical", "error":
		priority = "5"
	case "warning":
		priority = "4"
	case "info":
		priority = "2"
	}
	req, err := http.NewRequest("POST", strings.TrimRight(base, "/")+"/"+topic, strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", icon)
	if token := os.Getenv("NTFY_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("ntfy: %v", err)
		return
	}
	_ = resp.Body.Close()
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

// isLocal — ТОЛЬКО loopback. Раньше здесь были 10./172./192.168.*, но весь
// docker-трафик (в т.ч. gateway-контейнер) приходит из 172.*, поэтому такая
// проверка не защищала ничего: любой из контейнеров/LAN дёргал host-exec ручки.
// Сеть теперь закрыта токеном gateway (см. requireGatewayToken).
func isLocal(c fiber.Ctx) bool {
	return isLocalIP(c.IP())
}

// isLocalIP — вынесено отдельно, чтобы проверка была юнит-тестируемой:
// fiber.App.Test не прокидывает RemoteAddr в fasthttp.
func isLocalIP(ip string) bool {
	return ip == "127.0.0.1" || ip == "::1"
}

// gatewayToken — секрет для gateway→notify запросов (X-Gateway-Token).
// Приоритет: переменная окружения, затем NAS_STACK_DIR/.env — чтобы не держать
// копию секрета в systemd-юните и подхватывать ротацию токена из UI .env.
func gatewayToken() string {
	if t := strings.TrimSpace(platform.GetEnv("NOTIFY_GATEWAY_TOKEN", "")); t != "" {
		return t
	}
	repoDir := platform.GetEnv("NAS_STACK_DIR", "/home/konstantin/projects/nas-stack")
	data, err := os.ReadFile(filepath.Join(repoDir, ".env"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "NOTIFY_GATEWAY_TOKEN" {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

// gatewayTokenOK — сверяет заголовок X-Gateway-Token (или Bearer) с секретом.
func gatewayTokenOK(c fiber.Ctx) bool {
	expected := gatewayToken()
	if expected == "" {
		return false // fail-closed: без настроенного секрета никто не проходит
	}
	got := c.Get("X-Gateway-Token")
	if got == "" {
		if auth := c.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// requireGatewayToken — жёсткий режим для host-exec ручек
// (/update/run, /update/rollback, POST /env, /env/reload):
// только доверенный gateway с токеном, loopback тоже не пускает.
func requireGatewayToken(c fiber.Ctx) error {
	if gatewayTokenOK(c) {
		return c.Next()
	}
	return c.Status(403).JSON(fiber.Map{"error": "forbidden: gateway token required"})
}

// requireLocalOrToken — для ручек с ограниченным ущербом
// (GET /env с маскированными секретами, GET /oom-check, POST /send).
func requireLocalOrToken(c fiber.Ctx) error {
	if isLocal(c) || gatewayTokenOK(c) {
		return c.Next()
	}
	return c.Status(403).JSON(fiber.Map{"error": "forbidden"})
}

// isSecret — маскирует значения «секретных» переменных.
func isSecret(key string) bool {
	for _, s := range []string{"PASS", "SECRET", "TOKEN", "COOKIE", "API_KEY"} {
		if strings.Contains(key, s) {
			return true
		}
	}
	return false
}
