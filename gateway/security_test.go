package main

import (
	"net/http"
	"testing"
)

// Фикс 3b: gateway подставляет X-Gateway-Token для notify-service и
// перетирает клиентский заголовок (иначе клиент подсовывает свой токен).
func TestProxyHandlerInjectsGatewayToken(t *testing.T) {
	withSecrets(t, "admin-secret", "topsecret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	resp := doProxy(t, app, http.MethodPost, "/notify/update/run", nil, map[string]string{
		"Authorization": "Bearer admin-secret",
	})
	assertStatus(t, resp, http.StatusOK)

	calls, req := rec.last(t)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if v := req.Header.Get("X-Gateway-Token"); v != "topsecret" {
		t.Fatalf("upstream получил X-Gateway-Token=%q, ожидался %q", v, "topsecret")
	}
	if v := req.Header.Get("Authorization"); v != "" {
		t.Fatalf("JWT утёк в upstream: Authorization=%q", v)
	}
}

func TestProxyHandlerOverridesClientToken(t *testing.T) {
	withSecrets(t, "admin-secret", "topsecret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	resp := doProxy(t, app, http.MethodPost, "/notify/env", nil, map[string]string{
		"X-Gateway-Token": "attacker-token",
		"X-Admin-Token":   "admin-secret",
	})
	assertStatus(t, resp, http.StatusOK)

	calls, req := rec.last(t)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if v := req.Header.Get("X-Gateway-Token"); v != "topsecret" {
		t.Fatalf("клиентский токен не перетёрт: upstream получил %q", v)
	}
}

// Без настроенного секрета заголовок не отправляется (notify ответит 403
// и покажет это в логах — лучше, чем молча слать пустой токен).
func TestProxyHandlerNoTokenConfigured(t *testing.T) {
	withSecrets(t, "admin-secret", "")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "notify")

	resp := doProxy(t, app, http.MethodPost, "/notify/env", nil, map[string]string{
		"X-Admin-Token": "admin-secret",
	})
	assertStatus(t, resp, http.StatusOK)

	_, req := rec.last(t)
	if _, present := req.Header["X-Gateway-Token"]; present {
		t.Fatalf("заголовок отправлен без настроенного секрета: %q", req.Header.Get("X-Gateway-Token"))
	}
}

// Секрет не должен утекать в другие сервисы.
func TestProxyHandlerNoTokenToOtherServices(t *testing.T) {
	withSecrets(t, "admin-secret", "topsecret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "stats")

	resp := doProxy(t, app, http.MethodGet, "/stats/health", nil, nil)
	assertStatus(t, resp, http.StatusOK)

	_, req := rec.last(t)
	if _, present := req.Header["X-Gateway-Token"]; present {
		t.Fatalf("секрет ушёл в stats-service: %q", req.Header.Get("X-Gateway-Token"))
	}
}
