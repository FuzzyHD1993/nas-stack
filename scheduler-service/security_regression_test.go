package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

const testAdminToken = "topsecret-0123456789"

func callSched(t *testing.T, app *fiber.App, method, path, body string, hdr map[string]string) (int, string) {
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

// Все мутирующие ручки scheduler закрыты ADMIN_TOKEN: прямой доступ на :8084
// мимо gateway не должен запускать задачи и бэкапы.
func TestMutatingRoutesAreGuarded(t *testing.T) {
	app := newFiber()
	registerRoutes(app)

	guard := reflect.ValueOf(requireAdminToken).Pointer()
	want := []string{
		"POST /run/:name",
		"POST /gitea/backup",
		"POST /prowlarr/setup",
		"POST /backup/run",
		"POST /backup/restore",
		"POST /update/run",
	}

	routes := map[string][]fiber.Handler{}
	for _, r := range app.GetRoutes(true) {
		if r.Method == fiber.MethodHead {
			continue
		}
		routes[r.Method+" "+r.Path] = r.Handlers
	}
	for _, key := range want {
		hs, ok := routes[key]
		if !ok {
			t.Errorf("маршрут %s не зарегистрирован", key)
			continue
		}
		pos := -1
		for i, h := range hs {
			if reflect.ValueOf(h).Pointer() == guard {
				pos = i
				break
			}
		}
		if pos < 0 || pos >= len(hs)-1 {
			t.Errorf("%s: requireAdminToken отсутствует или стоит после обработчика", key)
		}
	}
}

func TestAdminTokenMatrix(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", testAdminToken)

	app := fiber.New()
	app.Post("/x", requireAdminToken, func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok"})
	})

	sameLenWrong := testAdminToken[:len(testAdminToken)-1] + "X"
	cases := []struct {
		name string
		path string
		hdr  map[string]string
		want int
	}{
		{"no header", "/x", nil, 403},
		{"wrong", "/x", map[string]string{"X-Admin-Token": "attacker"}, 403},
		{"same length wrong", "/x", map[string]string{"X-Admin-Token": sameLenWrong}, 403},
		{"valid x-admin", "/x", map[string]string{"X-Admin-Token": testAdminToken}, 200},
		{"valid bearer", "/x", map[string]string{"Authorization": "Bearer " + testAdminToken}, 200},
		{"lowercase bearer", "/x", map[string]string{"Authorization": "bearer " + testAdminToken}, 200},
		{"basic", "/x", map[string]string{"Authorization": "Basic " + testAdminToken}, 403},
		{"empty bearer", "/x", map[string]string{"Authorization": "Bearer "}, 403},
		{"query token", "/x?token=" + testAdminToken, nil, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := callSched(t, app, "POST", tc.path, "", tc.hdr); code != tc.want {
				t.Fatalf("код %d (%s), ожидался %d", code, body, tc.want)
			}
		})
	}
}

func TestAdminTokenFailsClosed(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "   ")
	app := fiber.New()
	app.Post("/x", requireAdminToken, func(c fiber.Ctx) error { return c.SendString("ok") })

	if code, _ := callSched(t, app, "POST", "/x", "", nil); code != 403 {
		t.Errorf("пустой ADMIN_TOKEN + нет заголовка = %d, ожидался 403", code)
	}
	if code, _ := callSched(t, app, "POST", "/x", "",
		map[string]string{"X-Admin-Token": "anything"}); code != 403 {
		t.Errorf("пустой ADMIN_TOKEN + произвольный токен = %d, ожидался 403", code)
	}
}

// Клиент не должен ходить по редиректу host-agent: X-Admin-Token утёк бы
// на произвольный хост.
func TestUpdateRunDoesNotFollowRedirect(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", testAdminToken)

	var victimHits atomic.Int32
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		victimHits.Add(1)
		if r.Header.Get("X-Admin-Token") != "" {
			t.Errorf("X-Admin-Token утёк на чужой хост")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer victim.Close()

	var agentHits atomic.Int32
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentHits.Add(1)
		if r.URL.Path != "/update/run" {
			t.Errorf("path = %q", r.URL.Path)
		}
		http.Redirect(w, r, victim.URL+"/latest/meta-data", http.StatusFound)
	}))
	defer agent.Close()

	t.Setenv("HOST_AGENT_URL", agent.URL)
	app := newFiber()
	registerRoutes(app)

	if code, body := callSched(t, app, "POST", "/update/run", "",
		map[string]string{"X-Admin-Token": "attacker"}); code != 403 {
		t.Fatalf("неверный токен = %d (%s)", code, body)
	}
	if agentHits.Load() != 0 {
		t.Fatalf("запрос ушёл на host-agent до проверки токена")
	}

	code, body := callSched(t, app, "POST", "/update/run", "",
		map[string]string{"X-Admin-Token": testAdminToken})
	if code != 502 {
		t.Fatalf("редирект от host-agent = %d (%s), ожидался 502", code, body)
	}
	if agentHits.Load() != 1 {
		t.Fatalf("host-agent вызван %d раз, ожидался 1", agentHits.Load())
	}
	if victimHits.Load() != 0 {
		t.Fatalf("клиент пошёл по редиректу (SSRF/token leak)")
	}
}

func TestProwlarrSetupIgnoresBodyURL(t *testing.T) {
	var attackerHits atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
	}))
	defer attacker.Close()

	var prowlarrHits atomic.Int32
	var seenKey atomic.Value
	prowlarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prowlarrHits.Add(1)
		seenKey.Store(r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer prowlarr.Close()

	t.Setenv("ADMIN_TOKEN", testAdminToken)
	t.Setenv("PROWLARR_URL", prowlarr.URL)
	t.Setenv("PROWLARR_API_KEY", "real-key")
	app := newFiber()
	registerRoutes(app)

	body := `{"url":"` + attacker.URL + `","api_key":"wizard-key"}`
	if code, resp := callSched(t, app, "POST", "/prowlarr/setup", body,
		map[string]string{"X-Admin-Token": testAdminToken}); code != 200 {
		t.Fatalf("POST /prowlarr/setup = %d (%s)", code, resp)
	}
	if attackerHits.Load() != 0 {
		t.Fatalf("URL из тела запроса использован (SSRF)")
	}
	if prowlarrHits.Load() < 3 {
		t.Fatalf("ожидались запросы к PROWLARR_URL, получено %d", prowlarrHits.Load())
	}
	if got, _ := seenKey.Load().(string); got != "wizard-key" {
		t.Fatalf("X-Api-Key = %q, ожидался ключ из авторизованного запроса", got)
	}

	// Без ключа в теле — fallback на env.
	if code, resp := callSched(t, app, "POST", "/prowlarr/setup", "",
		map[string]string{"X-Admin-Token": testAdminToken}); code != 200 {
		t.Fatalf("POST /prowlarr/setup без тела = %d (%s)", code, resp)
	}
	if got, _ := seenKey.Load().(string); got != "real-key" {
		t.Fatalf("X-Api-Key = %q, ожидался env-ключ", got)
	}
}

func TestProwlarrSetupRejectsInvalidEnvURL(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", testAdminToken)
	t.Setenv("PROWLARR_API_KEY", "k")
	t.Setenv("PROWLARR_URL", "file:///etc/passwd")
	app := newFiber()
	registerRoutes(app)

	if code, body := callSched(t, app, "POST", "/prowlarr/setup", "",
		map[string]string{"X-Admin-Token": testAdminToken}); code != 400 {
		t.Fatalf("PROWLARR_URL=file:// = %d (%s), ожидался 400", code, body)
	}
}

func TestSafeBackupPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BACKUP_DIR", dir)

	okFile := filepath.Join(dir, "nas_history-20260101-000000.sql.gz")
	if err := os.WriteFile(okFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := safeBackupPath(okFile); err != nil || got == "" {
		t.Fatalf("валидный путь отвергнут: %v", err)
	}

	bad := []string{
		filepath.Join(dir, "..", "etc", "passwd"),
		filepath.Join(dir, "evil';id;'.sql.gz"),
		filepath.Join(dir, "noext"),
	}
	for _, p := range bad {
		if _, err := safeBackupPath(p); err == nil {
			t.Errorf("небезопасный путь %q принят", p)
		}
	}

	link := filepath.Join(dir, "link.sql.gz")
	if err := os.Symlink("/etc/passwd", link); err == nil {
		if _, err := safeBackupPath(link); err == nil {
			t.Errorf("симлинк наружу принят")
		}
	}
}

func TestRestoreBackupRejectsNonGzip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BACKUP_DIR", dir)
	t.Setenv("POSTGRES_URL", "postgres://nas:secret@127.0.0.1:1/nas")
	p := filepath.Join(dir, "bad.sql.gz")
	if err := os.WriteFile(p, []byte("not a gzip dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(p, true); err == nil {
		t.Fatal("не-gzip файл принят как дамп")
	}
	if err := restoreBackup(p, false); err == nil {
		t.Fatal("restore без confirm=true не отклонён")
	}
}

func TestPgDSNStripsPassword(t *testing.T) {
	dsn, pass, err := pgDSN("postgres://nas:s3cr3t@postgres:5432/nas_history?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if pass != "s3cr3t" {
		t.Fatalf("password = %q", pass)
	}
	if strings.Contains(dsn, "s3cr3t") {
		t.Fatalf("пароль остался в DSN: %s", dsn)
	}
	if !strings.Contains(dsn, "sslmode=disable") {
		t.Fatalf("потерян sslmode: %s", dsn)
	}
}
