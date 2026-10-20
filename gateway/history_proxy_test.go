package main

import (
	"net/http"
	"testing"
)

// withHistoryToken подставляет тестовый сервисный токен history.
func withHistoryToken(t *testing.T, token string) {
	t.Helper()
	prev := currentHistoryToken()
	setHistoryToken(token)
	t.Cleanup(func() { setHistoryToken(prev) })
}

// lastHistoryRequest возвращает последний запрос и общее число вызовов.
func lastHistoryRequest(t *testing.T, rec *upstreamRecorder) (upstreamRequest, int) {
	t.Helper()
	calls, reqs := rec.snapshot()
	if len(reqs) == 0 {
		t.Fatalf("upstream: no requests recorded")
	}
	return reqs[len(reqs)-1], calls
}

func historyHeaderAbsent(t *testing.T, h http.Header, name string) {
	t.Helper()
	if vals := h.Values(name); len(vals) > 0 {
		t.Fatalf("%s present on upstream: %q", name, vals)
	}
}

// GET /history/* получает сервисный токен gateway, клиентский — перетирается.
func TestProxyHistoryInjectsServiceToken(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")
	withHistoryToken(t, "history-secret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "history")

	resp := doProxy(t, app, http.MethodGet, "/history/events", nil, map[string]string{
		"X-Gateway-Token": "attacker-gateway",
		"X-Admin-Token":   "attacker-admin",
	})
	assertStatus(t, resp, http.StatusOK)

	req, calls := lastHistoryRequest(t, rec)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if got := req.Header.Get("X-Gateway-Token"); got != "history-secret" {
		t.Fatalf("X-Gateway-Token = %q, want history-secret", got)
	}
	historyHeaderAbsent(t, req.Header, "X-Admin-Token")
	historyHeaderAbsent(t, req.Header, "Authorization")
}

// DELETE /history/events — только с ADMIN_TOKEN; upstream получает оба токена.
func TestProxyHistoryDeleteRequiresAdmin(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")
	withHistoryToken(t, "history-secret")

	rec, upstream := newRecordingUpstream(t)
	app := newNotifyApp(t, upstream.URL, "history")

	resp := doProxy(t, app, http.MethodDelete, "/history/events?all=1", nil, nil)
	assertStatus(t, resp, http.StatusUnauthorized)
	if calls, _ := rec.snapshot(); calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}

	resp = doProxy(t, app, http.MethodDelete, "/history/events?all=1", nil, map[string]string{
		"Authorization": "Bearer admin-secret",
	})
	assertStatus(t, resp, http.StatusOK)

	req, calls := lastHistoryRequest(t, rec)
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if got := req.Header.Get("X-Admin-Token"); got != "admin-secret" {
		t.Fatalf("X-Admin-Token = %q, want admin-secret", got)
	}
	if got := req.Header.Get("X-Gateway-Token"); got != "history-secret" {
		t.Fatalf("X-Gateway-Token = %q, want history-secret", got)
	}
}

func TestHistoryAdminPolicy(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "events", false},
		{http.MethodGet, "stats", false},
		{http.MethodDelete, "events", true},
		{http.MethodDelete, "", true},
	}
	for _, tc := range cases {
		if got := adminProtectedPath("history", tc.method, tc.path); got != tc.want {
			t.Errorf("adminProtectedPath(history,%q,%q) = %v, want %v",
				tc.method, tc.path, got, tc.want)
		}
	}
}
