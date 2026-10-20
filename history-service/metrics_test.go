package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
)

func metricsTestApp() *fiber.App {
	app := fiber.New()
	app.Use(metricsMiddleware())
	app.Use(recover.New())
	app.Get("/metrics-test/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
	app.Get("/metrics-test/teapot", func(c fiber.Ctx) error { return fiber.ErrTeapot })
	app.Get("/metrics-test/boom", func(c fiber.Ctx) error { return errors.New("boom") })
	app.Get("/metrics-test/panic", func(c fiber.Ctx) error { panic("boom") })
	return app
}

func metricsRequest(t *testing.T, app *fiber.App, target string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func hasSeries(t *testing.T, name, method, path, status string) bool {
	t.Helper()
	families, err := metricsRegistry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["method"] == method && labels["path"] == path && labels["status"] == status {
				return true
			}
		}
	}
	return false
}

func seriesCount(t *testing.T, name string) int {
	t.Helper()
	families, err := metricsRegistry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == name {
			return len(mf.GetMetric())
		}
	}
	return 0
}

func TestMetricsRecordsStatuses(t *testing.T) {
	app := metricsTestApp()

	if code := metricsRequest(t, app, "/metrics-test/ok"); code != fiber.StatusNoContent {
		t.Fatalf("ok status = %d", code)
	}
	if code := metricsRequest(t, app, "/metrics-test/teapot"); code != fiber.StatusTeapot {
		t.Fatalf("teapot status = %d", code)
	}
	if code := metricsRequest(t, app, "/metrics-test/boom"); code != fiber.StatusInternalServerError {
		t.Fatalf("boom status = %d", code)
	}
	if code := metricsRequest(t, app, "/metrics-test/panic"); code != fiber.StatusInternalServerError {
		t.Fatalf("panic status = %d", code)
	}

	if !hasSeries(t, "http_requests_total", "GET", "/metrics-test/ok", "204") {
		t.Error("204 не записан")
	}
	if !hasSeries(t, "http_requests_total", "GET", "/metrics-test/teapot", "418") {
		t.Error("ошибка хендлера не записана как 418")
	}
	if !hasSeries(t, "http_requests_total", "GET", "/metrics-test/boom", "500") {
		t.Error("errors.New не записан как 500")
	}
	if !hasSeries(t, "http_requests_total", "GET", "/metrics-test/panic", "500") {
		t.Error("паника не записана как 500")
	}
}

func TestMetricsUnmatchedRouteCollapsed(t *testing.T) {
	app := metricsTestApp()

	if code := metricsRequest(t, app, "/random-does-not-exist"); code != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	if !hasSeries(t, "http_requests_total", "GET", unmatchedRoute, "404") {
		t.Fatal("404 не попал в __unmatched__")
	}

	before := seriesCount(t, "http_requests_total")
	for i := 0; i < 100; i++ {
		metricsRequest(t, app, fmt.Sprintf("/random/%d", i))
	}
	after := seriesCount(t, "http_requests_total")
	if after != before {
		t.Fatalf("кардинальность выросла: %d -> %d (сырой путь попал в label)", before, after)
	}
}

func TestMetricsRegistryIsIsolated(t *testing.T) {
	rec := httptest.NewRecorder()
	metricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "go_goroutines") || strings.Contains(body, "process_resident_memory_bytes") {
		t.Fatalf("в /metrics попали go_*/process_*:\n%s", body)
	}
}
