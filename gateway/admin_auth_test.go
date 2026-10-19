package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// withSecrets подставляет тестовые секреты на время теста.
func withSecrets(t *testing.T, admin, notify string) {
	t.Helper()
	prevAdmin, prevNotify := adminToken, notifyToken
	adminToken, notifyToken = admin, notify
	t.Cleanup(func() { adminToken, notifyToken = prevAdmin, prevNotify })
}

// newNotifyApp — приложение с прокси на подставной upstream.
func newNotifyApp(t *testing.T, upstreamURL, service string) *fiber.App {
	t.Helper()
	app := fiber.New()
	app.All("/"+service+"/*", proxyHandler(upstreamURL, service))
	return app
}

func do(t *testing.T, app *fiber.App, method, target string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test %s %s: %v", method, target, err)
	}
	return resp
}

// P0 host-exec: без ADMIN_TOKEN ручки update/env не проксируются ВООБЩЕ —
// запрос не должен доходить до сервиса (проверяем счётчик вызовов upstream).
func TestAdminRoutesRequireToken(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	cases := []struct {
		name, method, url, service, path string
	}{
		{"notify update/run", "POST", "/notify/update/run", "notify", "update/run"},
		{"notify update/rollback", "POST", "/notify/update/rollback", "notify", "update/rollback"},
		{"notify env GET", "GET", "/notify/env", "notify", "env"},
		{"notify env POST", "POST", "/notify/env", "notify", "env"},
		{"notify env/reload", "POST", "/notify/env/reload", "notify", "env/reload"},
		{"scheduler update/run", "POST", "/scheduler/update/run", "scheduler", "update/run"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer upstream.Close()

			app := newNotifyApp(t, upstream.URL, tc.service)
			resp := do(t, app, tc.method, tc.url, nil)
			if resp.StatusCode != 401 {
				t.Fatalf("без токена: статус %d, ожидался 401", resp.StatusCode)
			}
			if calls != 0 {
				t.Fatalf("запрос без токена дошёл до сервиса (%d вызовов)", calls)
			}

			// С валидным токеном — 200 и запрос уходит.
			resp = do(t, app, tc.method, tc.url, map[string]string{"Authorization": "Bearer admin-secret"})
			if resp.StatusCode != 200 {
				t.Fatalf("с токеном: статус %d, ожидался 200", resp.StatusCode)
			}
			if calls != 1 {
				t.Fatalf("с валидным токеном сервис не вызван (calls=%d)", calls)
			}

			// Неверный токен — тоже 401.
			resp = do(t, app, tc.method, tc.url, map[string]string{"Authorization": "Bearer wrong"})
			if resp.StatusCode != 401 {
				t.Fatalf("с неверным токеном: статус %d, ожидался 401", resp.StatusCode)
			}
			if calls != 1 {
				t.Fatalf("неверный токен дошёл до сервиса (calls=%d)", calls)
			}
		})
	}
}

// Клиентский X-Admin-Token тоже принимается (dashboard/скрипты).
func TestAdminRoutesAcceptXAdminTokenHeader(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	app := newNotifyApp(t, upstream.URL, "notify")
	if resp := do(t, app, "POST", "/notify/env", map[string]string{"X-Admin-Token": "admin-secret"}); resp.StatusCode != 200 {
		t.Fatalf("X-Admin-Token: статус %d, ожидался 200", resp.StatusCode)
	}
}

// Слепой подстановки больше нет: обычные /notify/* не получают сервисный секрет.
// Иначе любой, кто дотянулся до gateway, авторизовался бы в notify-service.
func TestNoBlindServiceToken(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Gateway-Token") + "|" + r.Header.Get("X-Admin-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	app := newNotifyApp(t, upstream.URL, "notify")
	for _, target := range []string{"/notify/health", "/notify/oom-check", "/notify/send/not-a-handler"} {
		do(t, app, "GET", target, nil)
		if got != "|" {
			t.Fatalf("%s: сервисный секрет подставлен без adminAuth (%q)", target, got)
		}
	}
}

// /notify/send — уведомления: adminAuth не нужен, но сервисный токен подставляется.
func TestNotifySendGetsServiceTokenWithoutAdmin(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	var gotToken, gotAdmin string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Gateway-Token")
		gotAdmin = r.Header.Get("X-Admin-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	app := newNotifyApp(t, upstream.URL, "notify")
	if resp := do(t, app, "POST", "/notify/send", map[string]string{"Content-Type": "application/json"}); resp.StatusCode != 200 {
		t.Fatalf("/notify/send без admin-токена: статус %d, ожидался 200", resp.StatusCode)
	}
	if gotToken != "notify-secret" {
		t.Fatalf("/notify/send: X-Gateway-Token=%q, ожидался %q", gotToken, "notify-secret")
	}
	if gotAdmin != "" {
		t.Fatalf("/notify/send: X-Admin-Token не должен подставляться, получено %q", gotAdmin)
	}
}

// Клиентский сервисный токен не должен доезжать до сервиса.
func TestClientCannotInjectServiceToken(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Gateway-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	app := newNotifyApp(t, upstream.URL, "notify")
	// Клиент шлёт свой X-Gateway-Token — gateway обязан его перебить своим.
	do(t, app, "POST", "/notify/send", map[string]string{"X-Gateway-Token": "attacker"})
	if got != "notify-secret" {
		t.Fatalf("клиентский X-Gateway-Token не перебит: сервис получил %q", got)
	}

	// В админскую ручку клиентский сервисный токен тоже не проходит.
	got = ""
	do(t, app, "POST", "/notify/env", map[string]string{
		"X-Gateway-Token": "attacker", "X-Admin-Token": "admin-secret"})
	if got != "notify-secret" {
		t.Fatalf("админ-ручка: сервис получил X-Gateway-Token=%q, ожидался %q", got, "notify-secret")
	}
}

// Пустой ADMIN_TOKEN = fail-closed: никто не проходит.
func TestEmptyAdminTokenFailsClosed(t *testing.T) {
	withSecrets(t, "", "notify-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	app := newNotifyApp(t, upstream.URL, "notify")
	for _, h := range []map[string]string{
		nil,
		{"Authorization": "Bearer "},
		{"X-Admin-Token": "anything"},
	} {
		resp := do(t, app, "POST", "/notify/update/run", h)
		if resp.StatusCode != 401 {
			t.Fatalf("без ADMIN_TOKEN статус %d, ожидался 401", resp.StatusCode)
		}
	}
}

func TestAdminProtectedPathTable(t *testing.T) {
	cases := []struct {
		service, method, path string
		want                  bool
	}{
		{"notify", "POST", "update/run", true},
		{"notify", "POST", "update/rollback", true},
		{"notify", "GET", "env", true},
		{"notify", "POST", "env", true},
		{"notify", "POST", "env/reload", true},
		{"notify", "GET", "health", false},
		{"notify", "GET", "oom-check", false},
		{"notify", "POST", "send", false},
		{"scheduler", "POST", "update/run", true},
		{"scheduler", "GET", "update/status", false}, // read-only: git-метаданные
		{"scheduler", "POST", "update/status", true},
		{"scheduler", "GET", "backup/list", false},
		{"stats", "POST", "update/run", false}, // у stats нет таких ручек
	}
	for _, tc := range cases {
		if got := adminProtectedPath(tc.service, tc.method, tc.path); got != tc.want {
			t.Errorf("adminProtectedPath(%q,%q,%q) = %v, ожидалось %v", tc.service, tc.method, tc.path, got, tc.want)
		}
	}
}

// Middleware adminAuth: 401 без токена, 200 с корректным.
func TestAdminAuthMiddleware(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	app := fiber.New()
	app.Use("/notify/update/*", adminAuth)
	app.Use("/notify/env", adminAuth)
	app.All("/notify/update/*", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.All("/notify/env", func(c fiber.Ctx) error { return c.SendString("ok") })

	if resp := do(t, app, "POST", "/notify/update/run", nil); resp.StatusCode != 401 {
		t.Fatalf("middleware без токена: %d, ожидался 401", resp.StatusCode)
	}
	if resp := do(t, app, "GET", "/notify/env", map[string]string{"Authorization": "Bearer admin-secret"}); resp.StatusCode != 200 {
		t.Fatalf("middleware с токеном: %d, ожидался 200", resp.StatusCode)
	}
	if resp := do(t, app, "POST", "/notify/update/rollback", map[string]string{"Authorization": "Bearer nope"}); resp.StatusCode != 401 {
		t.Fatalf("middleware с неверным токеном: %d, ожидался 401", resp.StatusCode)
	}
}

// serviceTokenPath: только /notify/send, только POST/PUT.
func TestServiceTokenPathTable(t *testing.T) {
	cases := []struct {
		service, method, path string
		want                  bool
	}{
		{"notify", "POST", "send", true},
		{"notify", "PUT", "/send", true},
		{"notify", "GET", "send", false},
		{"notify", "POST", "send/extra", false},
		{"scheduler", "POST", "send", false},
		{"notify", "POST", "update/run", false},
	}
	for _, tc := range cases {
		if got := serviceTokenPath(tc.service, tc.method, tc.path); got != tc.want {
			t.Errorf("serviceTokenPath(%q,%q,%q) = %v, ожидалось %v", tc.service, tc.method, tc.path, got, tc.want)
		}
	}
}
