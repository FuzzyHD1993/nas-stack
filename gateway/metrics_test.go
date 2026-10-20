package main

import (
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Тесты в пакете не параллелятся: коллекторы глобальные.
func resetHTTPMetrics() {
	httpDuration.Reset()
	httpTotal.Reset()
	activeRequests.Set(0)
}

// newMetricsApp повторяет порядок middleware из main(): metrics снаружи,
// recover внутри — иначе паника учитывается как 200.
func newMetricsApp() *fiber.App {
	app := fiber.New()
	app.Use(metricsMiddleware())
	app.Use(recover.New())
	return app
}

// Сырые пути не должны создавать серии: все unmatched схлопываются в одну.
func TestMetricsCollapsesUnmatchedPaths(t *testing.T) {
	resetHTTPMetrics()

	app := newMetricsApp()
	app.Get("/known", func(c fiber.Ctx) error { return c.SendString("ok") })

	for _, target := range []string{"/a", "/b", "/random/1", "/random/2?x=1"} {
		assertStatus(t, doProxy(t, app, http.MethodGet, target, nil, nil), http.StatusNotFound)
	}

	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", unmatchedRoute, "404")); got != 4 {
		t.Fatalf("unmatched total = %v, want 4", got)
	}
	if got := testutil.CollectAndCount(httpTotal); got != 1 {
		t.Fatalf("series = %d, want 1", got)
	}
}

func TestMetricsUsesRouteTemplate(t *testing.T) {
	resetHTTPMetrics()

	app := newMetricsApp()
	app.Get("/media/:id", func(c fiber.Ctx) error { return c.SendString("ok") })

	assertStatus(t, doProxy(t, app, http.MethodGet, "/media/42", nil, nil), http.StatusOK)

	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", "/media/:id", "200")); got != 1 {
		t.Fatalf("route template metric = %v, want 1", got)
	}
}

func TestMetricsFiberErrorStatus(t *testing.T) {
	resetHTTPMetrics()

	app := newMetricsApp()
	app.Get("/missing", func(c fiber.Ctx) error { return fiber.ErrNotFound })

	assertStatus(t, doProxy(t, app, http.MethodGet, "/missing", nil, nil), http.StatusNotFound)

	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", "/missing", "404")); got != 1 {
		t.Fatalf("fiber.Error 404 metric = %v, want 1", got)
	}
}

func TestMetricsAttributesAuthMiddleware(t *testing.T) {
	resetHTTPMetrics()

	app := newMetricsApp()
	app.Use("/notify/*", func(c fiber.Ctx) error {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "no"})
	})

	assertStatus(t, doProxy(t, app, http.MethodGet, "/notify/health", nil, nil), http.StatusUnauthorized)

	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", "/notify/*", "401")); got != 1 {
		t.Fatalf("401 attribution = %v, want 1", got)
	}
}

func TestMetricsPanicIs500(t *testing.T) {
	resetHTTPMetrics()

	app := newMetricsApp()
	app.Get("/boom", func(c fiber.Ctx) error { panic("boom") })

	assertStatus(t, doProxy(t, app, http.MethodGet, "/boom", nil, nil), http.StatusInternalServerError)

	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", "/boom", "500")); got != 1 {
		t.Fatalf("panic metric = %v, want 1", got)
	}
	if got := testutil.ToFloat64(activeRequests); got != 0 {
		t.Fatalf("active requests = %v, want 0", got)
	}
}

func TestMetricLabelsAreBounded(t *testing.T) {
	if got := metricMethod("evil"); got != "OTHER" {
		t.Fatalf("metricMethod(evil) = %q, want OTHER", got)
	}
	if got := metricMethod(" get "); got != "GET" {
		t.Fatalf("metricMethod(get) = %q, want GET", got)
	}
	for _, code := range []int{-1, 0, 99, 1000} {
		if got := metricStatus(code); got != "unknown" {
			t.Fatalf("metricStatus(%d) = %q, want unknown", code, got)
		}
	}
	if got := metricStatus(404); got != "404" {
		t.Fatalf("metricStatus(404) = %q, want 404", got)
	}
}
