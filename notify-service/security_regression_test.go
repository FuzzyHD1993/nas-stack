package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

const testGatewayToken = "topsecret-0123456789"

func testApp(t *testing.T) *fiber.App {
	t.Helper()
	t.Setenv("NAS_STACK_DIR", t.TempDir())
	t.Setenv("NOTIFY_GATEWAY_TOKEN", testGatewayToken)
	t.Setenv("NTFY_URL", "")
	t.Setenv("NTFY_TOKEN", "")
	app := fiber.New(fiber.Config{BodyLimit: 64 << 10})
	registerRoutes(app, nil)
	return app
}

func testCall(t *testing.T, app *fiber.App, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, string(b)
}

func TestIsSecretSegments(t *testing.T) {
	cases := map[string]bool{
		"JWT_SECRET":        true,
		"RUTRACKER_PASS":    true,
		"NTFY_TOKEN":        true,
		"PROWLARR_API_KEY":  true,
		"AWS_ACCESS_KEY_ID": true,
		"SSH_PRIVATE_KEY":   true,
		"PASSWORD_HASH":     true,
		"TOKEN_TTL":         true,
		"GPG_KEY":           true,
		"db_password":       true,
		"ssh_private_key":   true,
		"FLIBUSTA_URL":      false,
		"MEDIA_MOVIES_DIR":  false,
		"COMPASS_DIR":       false,
		"MONKEY":            false,
		"PATH":              false,
	}
	for key, want := range cases {
		if got := isSecret(key); got != want {
			t.Errorf("isSecret(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestTruncateNonPositive(t *testing.T) {
	for _, max := range []int{-10, -1, 0} {
		if got := truncate("abc", max); got != "" {
			t.Errorf("truncate(\"abc\", %d) = %q, want \"\"", max, got)
		}
	}
	if got := truncate("Привет мир", 3); got != "При..." {
		t.Errorf("truncate = %q, want При...", got)
	}
	if got := tail([]byte("abc"), -1); got != "" {
		t.Errorf("tail(max=-1) = %q, want \"\"", got)
	}
}

func TestEnvGetDropsInvalidKeys(t *testing.T) {
	app := testApp(t)
	dir := os.Getenv("NAS_STACK_DIR")
	content := strings.Join([]string{
		"NOTIFY_GATEWAY_TOKEN=" + testGatewayToken,
		"A');alert(1)//=x",
		"MEDIA_MOVIES_DIR=/mnt/movies",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	code, body := testCall(t, app, http.MethodGet, "/env", "",
		map[string]string{"X-Gateway-Token": testGatewayToken})
	if code != 200 {
		t.Fatalf("GET /env = %d (%s)", code, body)
	}
	if strings.Contains(body, "alert(1)") {
		t.Errorf("невалидный ключ из .env попал в ответ (XSS): %s", body)
	}
	if strings.Contains(body, testGatewayToken) {
		t.Errorf("секрет утёк в GET /env: %s", body)
	}
	if !strings.Contains(body, "MEDIA_MOVIES_DIR") || !strings.Contains(body, "/mnt/movies") {
		t.Errorf("несекретная переменная потерялась: %s", body)
	}
}

func TestGatewayTokenBearerVariants(t *testing.T) {
	app := testApp(t)
	body := `{"key":"A","value":"1"}`

	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no token", nil, 403},
		{"wrong bearer", map[string]string{"Authorization": "Bearer attacker"}, 403},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, 403},
		{"basic", map[string]string{"Authorization": "Basic " + testGatewayToken}, 403},
		{"lowercase bearer", map[string]string{"Authorization": "bearer " + testGatewayToken}, 200},
		{"valid bearer", map[string]string{"Authorization": "Bearer " + testGatewayToken}, 200},
		{"x-gateway wins", map[string]string{
			"X-Gateway-Token": "attacker", "Authorization": "Bearer " + testGatewayToken,
		}, 403},
		{"empty x-gateway + bearer", map[string]string{
			"X-Gateway-Token": "", "Authorization": "Bearer " + testGatewayToken,
		}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, resp := testCall(t, app, http.MethodPost, "/env", body, tc.hdr); code != tc.want {
				t.Fatalf("статус = %d (%s), ожидался %d", code, resp, tc.want)
			}
		})
	}
}

func TestProtectedEnvKeysRejected(t *testing.T) {
	app := testApp(t)
	hdr := map[string]string{"X-Gateway-Token": testGatewayToken}
	for _, key := range []string{"PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "BASH_ENV", "COMPOSE_FILE", "DOCKER_HOST"} {
		t.Run(key, func(t *testing.T) {
			body := `{"key":"` + key + `","value":"/tmp/x"}`
			if code, resp := testCall(t, app, http.MethodPost, "/env", body, hdr); code != 400 {
				t.Errorf("POST /env %s = %d (%s), ожидался 400", key, code, resp)
			}
		})
	}
}

func TestGatewayTokenLastDuplicateWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NAS_STACK_DIR", dir)
	t.Setenv("NOTIFY_GATEWAY_TOKEN", "")
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("NOTIFY_GATEWAY_TOKEN=first\nNOTIFY_GATEWAY_TOKEN=last\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gatewayToken(); got != "last" {
		t.Fatalf("gatewayToken() = %q, want last (как docker compose)", got)
	}
}
