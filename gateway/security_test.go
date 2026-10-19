package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// Фикс 3b: gateway подставляет X-Gateway-Token для notify-service и
// перетирает клиентский заголовок (иначе клиент подсовывает свой токен).
func TestProxyHandlerInjectsGatewayToken(t *testing.T) {
	var gotToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Gateway-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	prev := notifyToken
	notifyToken = "topsecret"
	t.Cleanup(func() { notifyToken = prev })
	prevAdmin := adminToken
	adminToken = "admin-secret"
	t.Cleanup(func() { adminToken = prevAdmin })

	app := fiber.New()
	app.All("/notify/*", proxyHandler(upstream.URL, "notify"))

	req := httptest.NewRequest(http.MethodPost, "/notify/update/run", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("статус %d", resp.StatusCode)
	}
	if gotToken != "topsecret" {
		t.Fatalf("upstream получил X-Gateway-Token=%q, ожидался %q", gotToken, "topsecret")
	}
}

func TestProxyHandlerOverridesClientToken(t *testing.T) {
	var gotToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Gateway-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	prev := notifyToken
	notifyToken = "topsecret"
	t.Cleanup(func() { notifyToken = prev })
	prevAdmin := adminToken
	adminToken = "admin-secret"
	t.Cleanup(func() { adminToken = prevAdmin })

	app := fiber.New()
	app.All("/notify/*", proxyHandler(upstream.URL, "notify"))

	req := httptest.NewRequest(http.MethodPost, "/notify/env", nil)
	req.Header.Set("X-Gateway-Token", "attacker-token")
	req.Header.Set("X-Admin-Token", "admin-secret")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("статус %d", resp.StatusCode)
	}
	if gotToken != "topsecret" {
		t.Fatalf("клиентский токен не перетёрт: upstream получил %q", gotToken)
	}
}

// Без настроенного секрета заголовок не отправляется (notify ответит 403
// и покажет это в логах — лучше, чем молча слать пустой токен).
func TestProxyHandlerNoTokenConfigured(t *testing.T) {
	var gotToken string
	var present bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Gateway-Token"]
		gotToken = r.Header.Get("X-Gateway-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	prev := notifyToken
	notifyToken = ""
	t.Cleanup(func() { notifyToken = prev })
	prevAdmin := adminToken
	adminToken = "admin-secret"
	t.Cleanup(func() { adminToken = prevAdmin })

	app := fiber.New()
	app.All("/notify/*", proxyHandler(upstream.URL, "notify"))
	req := httptest.NewRequest(http.MethodPost, "/notify/env", nil)
	req.Header.Set("X-Admin-Token", "admin-secret")
	if _, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if present {
		t.Fatalf("заголовок отправлен без настроенного секрета: %q", gotToken)
	}
}

// Секрет не должен утекать в другие сервисы.
func TestProxyHandlerNoTokenToOtherServices(t *testing.T) {
	var gotToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Gateway-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	prev := notifyToken
	notifyToken = "topsecret"
	t.Cleanup(func() { notifyToken = prev })

	app := fiber.New()
	app.All("/stats/*", proxyHandler(upstream.URL, "stats"))
	if _, err := app.Test(httptest.NewRequest(http.MethodGet, "/stats/health", nil),
		fiber.TestConfig{Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if gotToken != "" {
		t.Fatalf("секрет ушёл в stats-service: %q", gotToken)
	}
}
