//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// TestProxyHandlerIntegration проверяет реальный reverse-proxy на фейковом upstream.
func TestProxyHandlerIntegration(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer upstream.Close()

	app := fiber.New()
	app.All("/stats/*", proxyHandler(upstream.URL, "stats"))

	req := httptest.NewRequest(http.MethodGet, "/stats/health?probe=1", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	data, _ := io.ReadAll(resp.Body)
	if string(data) != "{\"status\":\"ok\"}\n" {
		t.Errorf("body = %q", string(data))
	}
	if gotPath != "/health" || gotQuery != "probe=1" {
		t.Errorf("upstream got path=%q query=%q, want /health probe=1", gotPath, gotQuery)
	}
}
