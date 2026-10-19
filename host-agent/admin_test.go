package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// --- stackEnvValue: env → REPO_DIR/.env ---

func TestStackEnvValuePrefersEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REPO_DIR", dir)
	t.Setenv("__AGENT_TEST_VAR__", "from-env")
	if got := stackEnvValue("__AGENT_TEST_VAR__", "def"); got != "from-env" {
		t.Fatalf("stackEnvValue = %q, want from-env", got)
	}
}

func TestStackEnvValueFromDotenv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REPO_DIR", dir)
	t.Setenv("__AGENT_TEST_VAR__", "")
	envFile := filepath.Join(dir, ".env")
	content := "# comment\nA=1\n__AGENT_TEST_VAR__=\"s3cr3t\"\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stackEnvValue("__AGENT_TEST_VAR__", "def"); got != "s3cr3t" {
		t.Fatalf("stackEnvValue из .env = %q, want s3cr3t", got)
	}
	if got := stackEnvValue("__AGENT_TEST_MISSING__", "def"); got != "def" {
		t.Fatalf("stackEnvValue default = %q", got)
	}
}

// --- requireAdminToken: ручки с побочными эффектами на хосте ---

func newAgentAdminApp() *fiber.App {
	app := fiber.New()
	app.Post("/update/run", requireAdminToken, func(c fiber.Ctx) error { return c.SendString("started") })
	app.Post("/cleanup/run", requireAdminToken, func(c fiber.Ctx) error { return c.SendString("cleaned") })
	app.Post("/duplicates/resolve", requireAdminToken, func(c fiber.Ctx) error { return c.SendString("moved") })
	app.Get("/disks", func(c fiber.Ctx) error { return c.SendString("[]") }) // read-only, без токена
	return app
}

func TestAgentAdminTokenRequired(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "topsecret")
	t.Setenv("REPO_DIR", t.TempDir())

	app := newAgentAdminApp()
	for _, p := range []string{"/update/run", "/cleanup/run", "/duplicates/resolve"} {
		req := httptest.NewRequest(http.MethodPost, p, nil)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 403 {
			t.Errorf("POST %s без токена = %d, ожидался 403", p, resp.StatusCode)
		}

		req2 := httptest.NewRequest(http.MethodPost, p, nil)
		req2.Header.Set("X-Admin-Token", "topsecret")
		resp2, err := app.Test(req2, fiber.TestConfig{Timeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if resp2.StatusCode != 200 {
			t.Errorf("POST %s с ADMIN_TOKEN = %d, ожидался 200", p, resp2.StatusCode)
		}
	}

	// Bearer-вариант тоже принимается.
	req := httptest.NewRequest(http.MethodPost, "/update/run", nil)
	req.Header.Set("Authorization", "Bearer topsecret")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("Bearer ADMIN_TOKEN = %d, ожидался 200", resp.StatusCode)
	}

	// read-only без токена по-прежнему доступен.
	resp3, err := app.Test(httptest.NewRequest(http.MethodGet, "/disks", nil), fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp3.StatusCode != 200 {
		t.Errorf("GET /disks = %d, ожидался 200 (эндпоинт не под токеном)", resp3.StatusCode)
	}
}

func TestAgentAdminTokenFailsClosed(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("REPO_DIR", t.TempDir())
	app := newAgentAdminApp()
	req := httptest.NewRequest(http.MethodPost, "/update/run", nil)
	req.Header.Set("X-Admin-Token", "anything")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Errorf("без секрета даже 'anything' должен получать 403, получено %d", resp.StatusCode)
	}
}

// --- agentBindAddrs: loopback по умолчанию, список из HOST_AGENT_BIND ---

func TestAgentBindAddrsDefaultLoopback(t *testing.T) {
	t.Setenv("HOST_AGENT_BIND", "")
	// REPO_DIR указываем в пустую папку, иначе подхватится настоящий .env
	// проекта с HOST_AGENT_BIND и тест потеряет смысл.
	t.Setenv("REPO_DIR", t.TempDir())
	if got := agentBindAddrs(); len(got) != 1 || got[0] != "127.0.0.1:9998" {
		t.Fatalf("agentBindAddrs = %v, want [127.0.0.1:9998]", got)
	}
}

func TestAgentBindAddrsFromEnv(t *testing.T) {
	t.Setenv("HOST_AGENT_BIND", "127.0.0.1:9998, 192.168.0.4:9998")
	t.Setenv("REPO_DIR", t.TempDir())
	got := agentBindAddrs()
	if len(got) != 2 || got[0] != "127.0.0.1:9998" || got[1] != "192.168.0.4:9998" {
		t.Fatalf("agentBindAddrs = %v", got)
	}
}
