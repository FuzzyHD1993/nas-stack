package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// withSecrets подставляет тестовые секреты через тот же сеттер, что и main().
func withSecrets(t *testing.T, admin, notify string) {
	t.Helper()
	prevAdmin, prevNotify := currentAdminToken(), currentNotifyToken()
	setTokens(admin, notify)
	t.Cleanup(func() { setTokens(prevAdmin, prevNotify) })
}

// newNotifyApp собирает роутер так же, как прод: тот же список admin-префиксов
// и настоящий proxyHandler (не копия).
func newNotifyApp(t *testing.T, upstreamURL, service string) *fiber.App {
	t.Helper()
	app := fiber.New()
	registerAdminAuth(app)
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
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// P0 host-exec: без ADMIN_TOKEN ручки update/env не проксируются ВООБЩЕ.
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
		{"scheduler update/rollback", "POST", "/scheduler/update/rollback", "scheduler", "update/rollback"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, upstream := newRecordingUpstream(t)
			app := newNotifyApp(t, upstream.URL, tc.service)

			if resp := do(t, app, tc.method, tc.url, nil); resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("без токена: статус %d, ожидался 401", resp.StatusCode)
			}
			if calls, _ := rec.snapshot(); calls != 0 {
				t.Fatalf("запрос без токена дошёл до сервиса (%d вызовов)", calls)
			}

			if resp := do(t, app, tc.method, tc.url, map[string]string{
				"Authorization": "Bearer admin-secret",
			}); resp.StatusCode != http.StatusOK {
				t.Fatalf("с токеном: статус %d, ожидался 200", resp.StatusCode)
			}

			calls, reqs := rec.snapshot()
			if calls != 1 {
				t.Fatalf("с валидным токеном сервис не вызван (calls=%d)", calls)
			}
			if reqs[0].Path != "/"+tc.path {
				t.Fatalf("upstream path = %q, ожидался %q", reqs[0].Path, "/"+tc.path)
			}
			wantAdmin := ""
			if tc.service == "scheduler" {
				wantAdmin = "admin-secret"
			}
			if got := reqs[0].Header.Get("X-Admin-Token"); got != wantAdmin {
				t.Fatalf("X-Admin-Token = %q, ожидался %q", got, wantAdmin)
			}
			wantGW := ""
			if tc.service == "notify" {
				wantGW = "notify-secret"
			}
			if got := reqs[0].Header.Get("X-Gateway-Token"); got != wantGW {
				t.Fatalf("X-Gateway-Token = %q, ожидался %q", got, wantGW)
			}
			if got := reqs[0].Header.Get("Authorization"); got != "" {
				t.Fatalf("Authorization утёк на upstream: %q", got)
			}

			if resp := do(t, app, tc.method, tc.url, map[string]string{
				"Authorization": "Bearer wrong",
			}); resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("с неверным токеном: статус %d, ожидался 401", resp.StatusCode)
			}
			if calls, _ := rec.snapshot(); calls != 1 {
				t.Fatalf("неверный токен дошёл до сервиса (calls=%d)", calls)
			}
		})
	}
}

// Клиентский X-Admin-Token принимается, но в notify мастер-токен не уходит:
// notify авторизует по X-Gateway-Token.
func TestAdminRoutesAcceptXAdminTokenHeader(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	if resp := do(t, app, "POST", "/notify/env", map[string]string{
		"X-Admin-Token": "admin-secret",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("X-Admin-Token: статус %d, ожидался 200", resp.StatusCode)
	}
	_, reqs := rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("upstream вызван %d раз", len(reqs))
	}
	if got := reqs[0].Header.Get("X-Admin-Token"); got != "" {
		t.Fatalf("ADMIN_TOKEN не должен уходить в notify, получено %q", got)
	}
	if got := reqs[0].Header.Get("X-Gateway-Token"); got != "notify-secret" {
		t.Fatalf("X-Gateway-Token = %q, ожидался notify-secret", got)
	}

	if resp := do(t, app, "POST", "/notify/env", map[string]string{
		"X-Admin-Token": "wrong",
	}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("неверный X-Admin-Token: статус %d, ожидался 401", resp.StatusCode)
	}
}

// Слепой подстановки нет: обычные /notify/* не получают ни один секрет.
func TestNoBlindServiceToken(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	targets := []string{"/notify/health", "/notify/oom-check", "/notify/send/not-a-handler"}
	for i, target := range targets {
		if resp := do(t, app, "GET", target, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: статус %d, ожидался 200", target, resp.StatusCode)
		}

		calls, reqs := rec.snapshot()
		if calls != i+1 {
			t.Fatalf("%s: upstream вызван %d раз, ожидалось %d", target, calls, i+1)
		}
		req := reqs[len(reqs)-1]
		if got := req.Header.Get("X-Gateway-Token"); got != "" {
			t.Fatalf("%s: сервисный секрет подставлен без adminAuth (%q)", target, got)
		}
		if got := req.Header.Get("X-Admin-Token"); got != "" {
			t.Fatalf("%s: админ-секрет утёк без adminAuth (%q)", target, got)
		}
	}
}

// /notify/send — adminAuth не нужен, но сервисный токен подставляется,
// а клиентский X-Admin-Token отбрасывается.
func TestNotifySendGetsServiceTokenWithoutAdmin(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	if resp := do(t, app, "POST", "/notify/send", map[string]string{
		"Content-Type":  "application/json",
		"X-Admin-Token": "attacker",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("/notify/send без admin-токена: статус %d, ожидался 200", resp.StatusCode)
	}

	_, reqs := rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("upstream вызван %d раз", len(reqs))
	}
	if got := reqs[0].Header.Get("X-Gateway-Token"); got != "notify-secret" {
		t.Fatalf("/notify/send: X-Gateway-Token=%q", got)
	}
	if got := reqs[0].Header.Get("X-Admin-Token"); got != "" {
		t.Fatalf("/notify/send: X-Admin-Token не должен подставляться, получено %q", got)
	}
}

// Клиентский сервисный токен не должен доезжать до сервиса.
func TestClientCannotInjectServiceToken(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	do(t, app, "POST", "/notify/send", map[string]string{"X-Gateway-Token": "attacker"})
	_, reqs := rec.snapshot()
	if len(reqs) == 0 || reqs[0].Header.Get("X-Gateway-Token") != "notify-secret" {
		t.Fatalf("клиентский X-Gateway-Token не перебит: %v", reqs)
	}

	rec2, upstream2 := newRecordingUpstream(t)
	app2 := newNotifyApp(t, upstream2.URL, "notify")
	do(t, app2, "POST", "/notify/env", map[string]string{
		"X-Gateway-Token": "attacker", "X-Admin-Token": "admin-secret"})
	_, reqs2 := rec2.snapshot()
	if len(reqs2) == 0 || reqs2[0].Header.Get("X-Gateway-Token") != "notify-secret" {
		t.Fatalf("админ-ручка: клиентский X-Gateway-Token не перебит: %v", reqs2)
	}
}

// Пустой ADMIN_TOKEN = fail-closed: никто не проходит и upstream не дергается.
func TestEmptyAdminTokenFailsClosed(t *testing.T) {
	withSecrets(t, "", "notify-secret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	for _, h := range []map[string]string{
		nil,
		{"Authorization": "Bearer "},
		{"Authorization": "Bearer anything"},
		{"X-Admin-Token": "anything"},
	} {
		if resp := do(t, app, "POST", "/notify/update/run", h); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("без ADMIN_TOKEN статус %d, ожидался 401", resp.StatusCode)
		}
	}
	if calls, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("fail-closed нарушен: upstream вызван %d раз", calls)
	}
}

func TestAdminProtectedPathTable(t *testing.T) {
	cases := []struct {
		service, method, path string
		want                  bool
	}{
		{"notify", "POST", "update/run", true},
		{"notify", "POST", "update/rollback", true},
		{"notify", "POST", "update", true},
		{"notify", "POST", "update/", true},
		{"notify", "GET", "env", true},
		{"notify", "POST", "env", true},
		{"notify", "POST", "env/reload", true},
		{"scheduler", "POST", "update/run", true},
		{"scheduler", "GET", "update/status", false},
		{"scheduler", "HEAD", "update/status", false},
		{"scheduler", "POST", "update/status", true},
		{"scheduler", "POST", "run/pg-backup", true},
		{"scheduler", "POST", "gitea/backup", true},
		{"scheduler", "POST", "backup/run", true},
		{"scheduler", "POST", "backup/restore", true},
		{"scheduler", "POST", "prowlarr/setup", true},
		{"scheduler", "GET", "backup/list", false},
		{"scheduler", "GET", "update/status/", false},
		{"stats", "POST", "cleanup/run", true},
		{"stats", "POST", "update/run", true},
		{"stats", "GET", "cleanup/preview", false},
		{"media", "POST", "duplicates/resolve", true},
		{"media", "POST", "open/42", true},
		{"media", "POST", "rename/apply", true},
		{"media", "POST", "episode/7/open", true},
		{"media", "DELETE", "photos/cache", true},
		{"media", "POST", "progress", false},
		{"media", "POST", "scan", true},
		{"media", "POST", "scan/credits", true},
		{"media", "GET", "scan/status", false},

		// Границы prefix-логики.
		{"notify", "GET", "health", false},
		{"notify", "POST", "send", false},
		{"notify", "GET", "envfoo", false},
		{"notify", "GET", "environment", false},
		{"notify", "POST", "updatefoo", false},
		{"notify", "POST", "", false},
		{"notify", "POST", "%00", true},

		// canonicalProxyPath нормализует traversal — обе ручки остаются закрытыми.
		{"notify", "POST", "update/../env", true},
		{"notify", "POST", "env%2F..%2Fupdate", true},
	}
	for _, tc := range cases {
		if got := adminProtectedPath(tc.service, tc.method, tc.path); got != tc.want {
			t.Errorf("adminProtectedPath(%q,%q,%q) = %v, ожидалось %v",
				tc.service, tc.method, tc.path, got, tc.want)
		}
	}
}

// Middleware adminAuth: проверяется на том же списке префиксов, что и прод.
func TestAdminAuthMiddleware(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	app := fiber.New()
	registerAdminAuth(app)
	app.All("/*", func(c fiber.Ctx) error { return c.SendString("ok") })

	if resp := do(t, app, "POST", "/notify/update/run", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("middleware без токена: %d, ожидался 401", resp.StatusCode)
	}
	if resp := do(t, app, "GET", "/notify/env", map[string]string{
		"Authorization": "Bearer admin-secret",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("middleware с токеном: %d, ожидался 200", resp.StatusCode)
	}
	if resp := do(t, app, "POST", "/notify/update/rollback", map[string]string{
		"Authorization": "bearer admin-secret", // схема регистронезависима
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("middleware с lowercase bearer: %d, ожидался 200", resp.StatusCode)
	}
	if resp := do(t, app, "POST", "/notify/update/rollback", map[string]string{
		"Authorization": "Bearer nope",
	}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("middleware с неверным токеном: %d, ожидался 401", resp.StatusCode)
	}

	// Default-deny: любые mutating-ручки scheduler/stats/media закрыты.
	for _, target := range []string{
		"/scheduler/run/gitea-backup",
		"/scheduler/backup/restore",
		"/stats/cleanup/run",
		"/media/open/1",
		"/media/duplicates/resolve",
	} {
		if resp := do(t, app, "POST", target, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: статус %d, ожидался 401", target, resp.StatusCode)
		}
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
		{"notify", "POST", "send/", true},
		{"notify", "GET", "send", false},
		{"notify", "POST", "send/extra", false},
		{"scheduler", "POST", "send", false},
		{"notify", "POST", "update/run", false},
	}
	for _, tc := range cases {
		if got := serviceTokenPath(tc.service, tc.method, tc.path); got != tc.want {
			t.Errorf("serviceTokenPath(%q,%q,%q) = %v, ожидалось %v",
				tc.service, tc.method, tc.path, got, tc.want)
		}
	}
}

// Приоритет заголовков и парсинг токена.
func TestAdminHeaderParsing(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	_, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"валидный Bearer при мусорном X-Admin-Token", map[string]string{
			"X-Admin-Token": "wrong", "Authorization": "Bearer admin-secret"}, 200},
		{"lowercase bearer", map[string]string{"Authorization": "bearer admin-secret"}, 200},
		{"лишние пробелы", map[string]string{"Authorization": "Bearer   admin-secret "}, 200},
		{"хвост после токена", map[string]string{"Authorization": "Bearer admin-secret extra"}, 401},
		{"чужой X-Admin-Token", map[string]string{"X-Admin-Token": "attacker"}, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if resp := do(t, app, "POST", "/notify/env", tc.headers); resp.StatusCode != tc.want {
				t.Fatalf("статус %d, ожидался %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestSanitizeProxyHeader(t *testing.T) {
	if got := sanitizeProxyHeader("text/html\r\nX-Evil: 1"); got != "" {
		t.Fatalf("CRLF не отфильтрован: %q", got)
	}
	if got := sanitizeProxyHeader("application/json"); got != "application/json" {
		t.Fatalf("валидный заголовок изменён: %q", got)
	}
}
