package main

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestProxyHandlerIntegration проверяет реальный reverse-proxy на фейковом
// upstream: путь, query, тело и заголовки.
func TestProxyHandlerIntegration(t *testing.T) {
	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "stats")

	resp := doProxy(t, app, http.MethodGet, "/stats/health?probe=1&x=2&x=3", nil, map[string]string{
		"Authorization":   "Bearer must-not-leak",
		"X-Gateway-Token": "attacker-gateway",
		"X-Admin-Token":   "attacker-admin",
		"Host":            "evil.example",
	})
	assertStatus(t, resp, http.StatusOK)

	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		t.Errorf("content-type = %q (err=%v), want application/json", resp.Header.Get("Content-Type"), err)
	}

	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(readBody(t, resp), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Status != "ok" {
		t.Errorf("body.status = %q, want ok", payload.Status)
	}

	calls, req := rec.last(t)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if req.Path != "/health" {
		t.Errorf("upstream path = %q, want /health", req.Path)
	}

	query, err := url.ParseQuery(req.Query)
	if err != nil {
		t.Fatalf("upstream query %q: %v", req.Query, err)
	}
	if query.Get("probe") != "1" {
		t.Errorf("upstream probe = %q, want 1", query.Get("probe"))
	}
	if xs := query["x"]; len(xs) != 2 || xs[0] != "2" || xs[1] != "3" {
		t.Errorf("upstream x = %v, want [2 3]", xs)
	}
}

// Клиентские Authorization/Host/служебные заголовки не должны доезжать до upstream.
func TestProxyHandlerStripsSensitiveHeaders(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "stats")

	resp := doProxy(t, app, http.MethodGet, "/stats/health", nil, map[string]string{
		"Host":            "evil.example",
		"Authorization":   "Bearer client-jwt",
		"X-Gateway-Token": "attacker",
		"X-Admin-Token":   "attacker",
		"X-Custom":        "keep-me",
	})
	assertStatus(t, resp, http.StatusOK)

	calls, req := rec.last(t)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	for _, h := range []string{"Authorization", "X-Gateway-Token", "X-Admin-Token"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("%s утёк на upstream: %q", h, v)
		}
	}
	if v := req.Header.Get("X-Custom"); v != "keep-me" {
		t.Errorf("X-Custom = %q, want keep-me", v)
	}
	if want := upstreamHost(t, upstream.URL); req.Host != want {
		t.Errorf("upstream Host = %q, want %q (клиентский Host не должен влиять)", req.Host, want)
	}
}

// Cookie и method/URL-override не должны доезжать до upstream: gateway
// авторизует только по Authorization/JWT.
func TestProxyHandlerStripsCookieAndOverrideHeaders(t *testing.T) {
	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "stats")

	resp := doProxy(t, app, http.MethodGet, "/stats/health", nil, map[string]string{
		"Cookie":                 "session=secret",
		"X-HTTP-Method-Override": "POST",
		"X-Method-Override":      "POST",
		"X-Original-URL":         "/notify/env",
		"X-Rewrite-URL":          "/notify/env",
	})
	assertStatus(t, resp, http.StatusOK)

	_, req := rec.last(t)
	for _, h := range []string{
		"Cookie", "X-Http-Method-Override", "X-Method-Override",
		"X-Original-Url", "X-Rewrite-Url",
	} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("%s утёк на upstream: %q", h, v)
		}
	}
}

// Query пробрасывается без пере-кодирования (%2F, +, повторяющиеся ключи).
func TestProxyHandlerPreservesRawQuery(t *testing.T) {
	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "stats")

	const rawQuery = "a=%2F&b=x+y&c=1&c=2"
	resp := doProxy(t, app, http.MethodGet, "/stats/health?"+rawQuery, nil, nil)
	assertStatus(t, resp, http.StatusOK)

	_, req := rec.last(t)
	if req.Query != rawQuery {
		t.Errorf("upstream query = %q, want %q", req.Query, rawQuery)
	}
}

// Метод и тело пробрасываются для всех поддерживаемых методов.
func TestProxyHandlerForwardsMethodsAndBody(t *testing.T) {
	cases := []struct {
		name   string
		method string
		body   []byte
	}{
		{"GET", http.MethodGet, nil},
		{"POST", http.MethodPost, []byte(`{"n":1}`)},
		{"PUT", http.MethodPut, []byte(`{"n":2}`)},
		{"PATCH", http.MethodPatch, []byte(`{"n":3}`)},
		{"DELETE", http.MethodDelete, []byte(`{"n":4}`)},
	}

	withSecrets(t, "admin-secret", "notify-secret")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, upstream := newRecordingUpstream(t)
			app := newNotifyApp(t, upstream.URL, "stats")

			resp := doProxy(t, app, tc.method, "/stats/echo", tc.body,
				map[string]string{
					"Content-Type":  "application/json",
					"Authorization": "Bearer admin-secret",
				})
			assertStatus(t, resp, http.StatusOK)

			calls, req := rec.last(t)
			if calls != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls)
			}
			if req.Method != tc.method {
				t.Errorf("upstream method = %q, want %q", req.Method, tc.method)
			}
			if !bytes.Equal(req.Body, tc.body) {
				t.Errorf("upstream body = %q, want %q", req.Body, tc.body)
			}
		})
	}
}

// Неподдерживаемый метод не должен доходить до upstream.
func TestProxyHandlerRejectsUnsupportedMethod(t *testing.T) {
	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "stats")

	resp := doProxy(t, app, http.MethodOptions, "/stats/health", nil, nil)
	assertStatus(t, resp, http.StatusMethodNotAllowed)

	if calls, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

// Двойное кодирование переживает нормализацию fasthttp и отклоняется
// validateProxyPath.
func TestProxyHandlerRejectsEncodedTraversal(t *testing.T) {
	targets := []string{
		"/stats/..%2f..%2fadmin",
		"/stats/%2e%2e/%2e%2e/admin",
		"/stats/%252e%252e%252f%252e%252e%252fadmin",
		"/stats/..%5c..%5cadmin",
	}

	for i, target := range targets {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			rec, upstream := newRecordingUpstream(t)
			app := newNotifyApp(t, upstream.URL, "stats")

			resp := doProxy(t, app, http.MethodGet, target, nil, nil)
			// 401 возможен, если admin-guard сработал раньше (fail-closed).
			if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound &&
				resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 400/401/404", target, resp.StatusCode)
			}
			if calls, _ := rec.snapshot(); calls != 0 {
				t.Fatalf("%s: upstream calls = %d, want 0", target, calls)
			}
		})
	}
}

// Абсолютный URI в хвосте пути не меняет host upstream (нет SSRF через path).
func TestProxyHandlerDoesNotFollowAbsoluteURIPath(t *testing.T) {
	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "stats")

	resp := doProxy(t, app, http.MethodGet, "/stats/http:%2F%2Fevil.example/x", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	calls, req := rec.last(t)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if want := upstreamHost(t, upstream.URL); req.Host != want {
		t.Fatalf("SSRF: upstream Host = %q, want %q", req.Host, want)
	}
}

// Upstream недоступен → 502, а не 500/паника.
func TestProxyHandlerReturns502WhenUpstreamDown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL := upstream.URL
	upstream.Close()

	app := newNotifyApp(t, upstreamURL, "stats")
	resp := doProxy(t, app, http.MethodGet, "/stats/health", nil, nil)
	assertStatus(t, resp, http.StatusBadGateway)

	var payload struct {
		Error   string `json:"error"`
		Service string `json:"service"`
	}
	if err := json.Unmarshal(readBody(t, resp), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload.Error != "service unavailable" {
		t.Errorf("error = %q, want %q", payload.Error, "service unavailable")
	}
	if payload.Service != "stats" {
		t.Errorf("service = %q, want stats", payload.Service)
	}
}

// Host-exec ручки проксируемых сервисов закрыты ADMIN_TOKEN, в том числе при
// кодировании/нормализации пути.
func TestProxyIntegrationAdminProtectedPath(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	blocked := []string{
		"/notify/update/run",
		"/notify/update%2Frun",
		"/notify/update%252Frun",
		"/notify/./update/run",
		"/notify//update//run",
		"/notify/env",
		"/notify/%65nv",
	}

	for i, target := range blocked {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			rec, upstream := newRecordingUpstream(t)
			app := newNotifyApp(t, upstream.URL, "notify")

			resp := doProxy(t, app, http.MethodPost, target, nil, nil)
			assertStatus(t, resp, http.StatusUnauthorized)

			if calls, _ := rec.snapshot(); calls != 0 {
				t.Fatalf("%s: upstream calls = %d, want 0", target, calls)
			}
		})
	}

	t.Run("allowed_with_admin_token", func(t *testing.T) {
		rec, upstream := newRecordingUpstream(t)
		app := newNotifyApp(t, upstream.URL, "notify")

		resp := doProxy(t, app, http.MethodPost, "/notify/update/run", nil, map[string]string{
			"Authorization":   "Bearer admin-secret",
			"X-Gateway-Token": "attacker-gateway",
		})
		assertStatus(t, resp, http.StatusOK)

		calls, req := rec.last(t)
		if calls != 1 {
			t.Fatalf("upstream calls = %d, want 1", calls)
		}
		if req.Path != "/update/run" {
			t.Errorf("upstream path = %q, want /update/run", req.Path)
		}
		if v := req.Header.Get("Authorization"); v != "" {
			t.Errorf("Authorization утёк на upstream: %q", v)
		}
		if v := req.Header.Get("X-Gateway-Token"); v != "notify-secret" {
			t.Errorf("X-Gateway-Token = %q, want notify-secret", v)
		}
		if v := req.Header.Get("X-Admin-Token"); v != "" {
			t.Errorf("ADMIN_TOKEN не должен уходить в notify: %q", v)
		}
	})
}
