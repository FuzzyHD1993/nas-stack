package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// --- Фикс 3: isLocal больше не доверяет 172.*/10.*/192.168.* ---

func TestIsLocalOnlyLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,
		"::1":             true,
		"172.17.0.2":      false, // docker-мост: раньше проходил, теперь нет
		"172.18.0.5":      false,
		"10.0.0.1":        false,
		"192.168.0.4":     false,
		"192.168.1.77":    false,
		"8.8.8.8":         false,
		"169.254.169.254": false,
		"":                false,
	}
	for ip, want := range cases {
		if got := isLocalIP(ip); got != want {
			t.Errorf("isLocalIP(%q) = %v, want %v", ip, got, want)
		}
	}
}

// --- gatewayToken: переменная окружения → .env в NAS_STACK_DIR ---

func TestGatewayTokenFromEnvAndFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NAS_STACK_DIR", dir)
	t.Setenv("NOTIFY_GATEWAY_TOKEN", "")

	if got := gatewayToken(); got != "" {
		t.Fatalf("без секрета gatewayToken() = %q", got)
	}

	// Читается из NAS_STACK_DIR/.env (без дублирования секрета в systemd-юните).
	envFile := filepath.Join(dir, ".env")
	content := "A=1\n# NOTIFY_GATEWAY_TOKEN=commented\nNOTIFY_GATEWAY_TOKEN=\"s3cr3t\"\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gatewayToken(); got != "s3cr3t" {
		t.Fatalf("gatewayToken() из .env = %q, want s3cr3t", got)
	}

	// Приоритет у переменной окружения.
	t.Setenv("NOTIFY_GATEWAY_TOKEN", "from-env")
	if got := gatewayToken(); got != "from-env" {
		t.Fatalf("gatewayToken() = %q, want from-env", got)
	}
}

func newTokenApp() *fiber.App {
	app := fiber.New()
	app.Post("/update/run", requireGatewayToken, func(c fiber.Ctx) error {
		return c.SendString("started")
	})
	app.Post("/update/rollback", requireGatewayToken, func(c fiber.Ctx) error {
		return c.SendString("rollback_started")
	})
	app.Post("/env", requireGatewayToken, func(c fiber.Ctx) error {
		return c.SendString("saved")
	})
	app.Post("/env/reload", requireGatewayToken, func(c fiber.Ctx) error {
		return c.SendString("reloaded")
	})
	app.Get("/env", requireLocalOrToken, func(c fiber.Ctx) error {
		return c.SendString("vars")
	})
	app.Get("/oom-check", requireLocalOrToken, func(c fiber.Ctx) error {
		return c.SendString("lines")
	})
	return app
}

func do(app *fiber.App, method, path, token string) int {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("X-Gateway-Token", token)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		return -1
	}
	return resp.StatusCode
}

// Без токена host-exec недоступен ни loopback, ни из docker-сети.
func TestRequireGatewayTokenStrict(t *testing.T) {
	t.Setenv("NAS_STACK_DIR", t.TempDir())
	t.Setenv("NOTIFY_GATEWAY_TOKEN", "topsecret")

	app := newTokenApp()
	protected := []string{"/update/run", "/update/rollback", "/env", "/env/reload"}

	for _, path := range protected {
		if code := do(app, "POST", path, ""); code != 403 {
			t.Errorf("POST %s без токена = %d, ожидался 403", path, code)
		}
		if code := do(app, "POST", path, "wrong-token"); code != 403 {
			t.Errorf("POST %s с неверным токеном = %d, ожидался 403", path, code)
		}
		if code := do(app, "POST", path, "topsecret"); code != 200 {
			t.Errorf("POST %s с корректным токеном = %d, ожидался 200", path, code)
		}
	}
}

// Read-only ручки: loopback ИЛИ токен (token-ветка проверяется через app.Test).
func TestRequireLocalOrToken(t *testing.T) {
	t.Setenv("NAS_STACK_DIR", t.TempDir())
	t.Setenv("NOTIFY_GATEWAY_TOKEN", "topsecret")

	app := newTokenApp()

	if code := do(app, "GET", "/oom-check", ""); code != 403 {
		t.Errorf("GET /oom-check без токена = %d, ожидался 403", code)
	}
	if code := do(app, "GET", "/oom-check", "topsecret"); code != 200 {
		t.Errorf("GET /oom-check с токеном = %d, ожидался 200", code)
	}
	if code := do(app, "GET", "/env", "topsecret"); code != 200 {
		t.Errorf("GET /env с токеном = %d, ожидался 200", code)
	}
}

// Fail-closed: без настроенного секрета gateway-токен не проходит.
func TestGatewayTokenFailsClosedWhenUnset(t *testing.T) {
	t.Setenv("NAS_STACK_DIR", t.TempDir())
	t.Setenv("NOTIFY_GATEWAY_TOKEN", "")

	app := newTokenApp()
	if code := do(app, "POST", "/update/run", ""); code != 403 {
		t.Errorf("без секрета ожидался 403, получено %d", code)
	}
	if code := do(app, "POST", "/update/run", "anything"); code != 403 {
		t.Errorf("без секрета даже произвольный токен не должен проходить, получено %d", code)
	}
}

// Bearer-вариант тоже принимается (Authorization: Bearer ...).
func TestGatewayTokenBearerFallback(t *testing.T) {
	t.Setenv("NAS_STACK_DIR", t.TempDir())
	t.Setenv("NOTIFY_GATEWAY_TOKEN", "topsecret")

	app := newTokenApp()
	req := httptest.NewRequest("POST", "/update/run", nil)
	req.Header.Set("Authorization", "Bearer topsecret")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("Bearer-токен: статус %d, ожидался 200", resp.StatusCode)
	}
}

// Loopback-ветка — на реальном сокете: fiber.App.Test не прокидывает
// RemoteAddr в fasthttp, поэтому c.IP() там всегда пустой.
func TestRequireLocalOrTokenLoopbackRealConn(t *testing.T) {
	t.Setenv("NAS_STACK_DIR", t.TempDir())
	t.Setenv("NOTIFY_GATEWAY_TOKEN", "topsecret")

	app := newTokenApp()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() { _ = app.Listener(ln) }()

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 3 * time.Second}

	if code, body := getCode(t, client, base+"/env"); code != 200 {
		t.Errorf("GET /env с реального loopback = %d (%s), ожидался 200", code, body)
	}
	if code, _ := getCode(t, client, base+"/oom-check"); code != 200 {
		t.Errorf("GET /oom-check с loopback = %d, ожидался 200", code)
	}
	// Даже с loopback host-exec ручка без токена закрыта (жёсткий режим).
	if code, _ := postCode(t, client, base+"/update/run", ""); code != 403 {
		t.Errorf("POST /update/run с loopback без токена = %d, ожидался 403", code)
	}
	if code, _ := postCode(t, client, base+"/update/run", "topsecret"); code != 200 {
		t.Errorf("POST /update/run с loopback + токен = %d, ожидался 200", code)
	}
}

func getCode(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func postCode(t *testing.T, c *http.Client, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Gateway-Token", token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
