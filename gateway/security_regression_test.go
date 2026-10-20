package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Host-exec ручки scheduler нельзя вызвать без ADMIN_TOKEN.
func TestSchedulerHostExecRequiresAdmin(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	blocked := []struct{ method, target string }{
		{http.MethodPost, "/scheduler/run/pg-backup"},
		{http.MethodPost, "/scheduler/gitea/backup"},
		{http.MethodPost, "/scheduler/backup/run"},
		{http.MethodPost, "/scheduler/backup/restore"},
		{http.MethodPost, "/scheduler/prowlarr/setup"},
		{http.MethodPost, "/scheduler/update/run"},
	}
	for _, tc := range blocked {
		t.Run("blocked_"+tc.target, func(t *testing.T) {
			rec, upstream := newRecordingUpstream(t)
			app := newNotifyApp(t, upstream.URL, "scheduler")

			assertStatus(t, doProxy(t, app, tc.method, tc.target, nil, nil), http.StatusUnauthorized)

			if calls, _ := rec.snapshot(); calls != 0 {
				t.Fatalf("%s: upstream calls = %d, want 0", tc.target, calls)
			}
		})
	}

	allowed := []string{
		"/scheduler/health",
		"/scheduler/tasks",
		"/scheduler/update/status",
		"/scheduler/backup/list",
	}
	for _, target := range allowed {
		t.Run("allowed_"+target, func(t *testing.T) {
			rec, upstream := newRecordingUpstream(t)
			app := newNotifyApp(t, upstream.URL, "scheduler")

			assertStatus(t, doProxy(t, app, http.MethodGet, target, nil, nil), http.StatusOK)

			if calls, _ := rec.snapshot(); calls != 1 {
				t.Fatalf("%s: upstream calls = %d, want 1", target, calls)
			}
		})
	}

	t.Run("admin_token_allows_run", func(t *testing.T) {
		rec, upstream := newRecordingUpstream(t)
		app := newNotifyApp(t, upstream.URL, "scheduler")

		assertStatus(t, doProxy(t, app, http.MethodPost, "/scheduler/run/pg-backup", nil,
			map[string]string{"Authorization": "Bearer admin-secret"}), http.StatusOK)

		calls, req := rec.last(t)
		if calls != 1 {
			t.Fatalf("upstream calls = %d, want 1", calls)
		}
		if got := req.Header.Get("X-Admin-Token"); got != "admin-secret" {
			t.Fatalf("X-Admin-Token = %q, want admin-secret", got)
		}
	})
}

// Валидный JWT не заменяет ADMIN_TOKEN на host-exec ручках.
func TestValidJWTDoesNotGrantAdminAccess(t *testing.T) {
	const secret = "test-secret-0123456789abcdef01234567"
	withSecrets(t, "admin-secret", "notify-secret")

	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":   "user",
		"iss":   "nas-auth",
		"scope": "admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	rec, upstream := newRecordingUpstream(t)
	app := fiber.New()
	app.Use("/notify/*", requireAuthMiddleware([]byte(secret), true))
	registerAdminAuth(app)
	app.All("/notify/*", proxyHandler(upstream.URL, "notify"))

	resp := doProxy(t, app, http.MethodPost, "/notify/update/run", nil,
		map[string]string{"Authorization": "Bearer " + tok})
	assertStatus(t, resp, http.StatusUnauthorized)

	if calls, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("host-exec по JWT: upstream вызван %d раз", calls)
	}

	// Тот же JWT открывает обычную ручку — значит, отсекает именно adminGuard.
	assertStatus(t, doProxy(t, app, http.MethodGet, "/notify/health", nil,
		map[string]string{"Authorization": "Bearer " + tok}), http.StatusOK)
}

// Media host-exec/file-mutating ручки закрыты ADMIN_TOKEN.
func TestMediaHostExecRequiresAdmin(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	blocked := []struct{ method, target string }{
		{http.MethodPost, "/media/duplicates/resolve"},
		{http.MethodPost, "/media/open/42"},
		{http.MethodPost, "/media/episode/7/open"},
		{http.MethodPost, "/media/rename/apply"},
		{http.MethodDelete, "/media/photos/cache"},
	}
	for _, tc := range blocked {
		t.Run(tc.method+"_"+tc.target, func(t *testing.T) {
			rec, upstream := newRecordingUpstream(t)
			app := newNotifyApp(t, upstream.URL, "media")

			assertStatus(t, doProxy(t, app, tc.method, tc.target, nil, nil), http.StatusUnauthorized)

			if calls, _ := rec.snapshot(); calls != 0 {
				t.Fatalf("%s: upstream calls = %d, want 0", tc.target, calls)
			}
		})
	}

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "media")
	assertStatus(t, doProxy(t, app, http.MethodPost, "/media/progress", nil, nil), http.StatusOK)
	if calls, _ := rec.snapshot(); calls != 1 {
		t.Fatalf("обычный POST /media/progress: upstream calls = %d, want 1", calls)
	}
}

// Глубокое кодирование не обходит admin-политику.
func TestDeepEncodedAdminPaths(t *testing.T) {
	if got := canonicalProxyPath("update%2525252Frun"); got != "update/run" {
		t.Fatalf("canonicalProxyPath = %q, want update/run", got)
	}
	if !adminProtectedPath("notify", http.MethodPost, "update%2525252Frun") {
		t.Fatal("4-слойное кодирование обошло adminProtectedPath")
	}

	deep := "update" + strings.Repeat("%25", 12) + "2Frun"
	if err := validateProxyPath(deep); err == nil {
		t.Fatal("слишком глубокая кодировка должна отвергаться")
	}

	for _, p := range []string{"update%23x", "update%3Fx", "update%00"} {
		if err := validateProxyPath(p); err == nil {
			t.Errorf("validateProxyPath(%q) = nil, ожидалась ошибка", p)
		}
	}
}

// Hop-by-hop и клиентские forwarding-заголовки не доезжают до upstream.
func TestProxyStripsHopByHopHeaders(t *testing.T) {
	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "stats")

	resp := doProxy(t, app, http.MethodGet, "/stats/health", nil, map[string]string{
		"Connection":       "keep-alive, X-Nominated",
		"X-Nominated":      "1",
		"TE":               "trailers",
		"X-Real-IP":        "1.2.3.4",
		"X-Forwarded-For":  "1.2.3.4",
		"X-Forwarded-Host": "evil.example",
	})
	assertStatus(t, resp, http.StatusOK)

	calls, req := rec.last(t)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	for _, h := range []string{"Connection", "X-Nominated", "TE", "X-Real-IP", "X-Forwarded-Host"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("%s утёк на upstream: %q", h, v)
		}
	}
	if v := req.Header.Get("X-Forwarded-For"); v == "" || v == "1.2.3.4" {
		t.Errorf("X-Forwarded-For = %q, want доверенный IP gateway", v)
	}
}

// Upstream не может навязать чужой redirect или HTML-контекст.
func TestProxyResponseHardening(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Location", "https://evil.example/")
		w.Header().Set("Set-Cookie", "session=evil; Path=/")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`<script>alert(document.domain)</script>`))
	}))
	t.Cleanup(upstream.Close)

	app := newNotifyApp(t, upstream.URL, "stats")
	resp := doProxy(t, app, http.MethodGet, "/stats/health", nil, nil)
	assertStatus(t, resp, http.StatusFound)

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Fatalf("CSP = %q, want sandbox", got)
	}
	if got := resp.Header.Get("Location"); got != "" {
		t.Fatalf("open redirect: Location = %q, want empty", got)
	}
	if got := resp.Header.Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie проброшен на origin gateway: %q", got)
	}
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.HasPrefix(ct, "text/html") {
		t.Fatalf("активный Content-Type проброшен: %q", ct)
	}
}

func TestValidateCoverURL(t *testing.T) {
	bad := []string{
		"",
		"file:///etc/passwd",
		"gopher://127.0.0.1:6379/_INFO",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"http://127.0.0.1/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/x",
		"http://user@evil.example/x",
		"http://localhost/x",
		"http://host.containers.internal/x",
	}
	for _, raw := range bad {
		if err := validateCoverURL(raw); err == nil {
			t.Errorf("validateCoverURL(%q) = nil, want error", raw)
		}
	}

	good := []string{
		"https://covers.example.com/a.jpg",
		"http://8.8.8.8/a.jpg",
	}
	for _, raw := range good {
		if err := validateCoverURL(raw); err != nil {
			t.Errorf("validateCoverURL(%q) = %v, want nil", raw, err)
		}
	}
}
