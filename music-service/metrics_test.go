package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func resetHTTPMetrics() {
	httpDuration.Reset()
	httpTotal.Reset()
	activeRequests.Set(0)
}

// newMetricsApp повторяет боевой порядок: metrics снаружи recover.
func newMetricsApp() *fiber.App {
	app := fiber.New()
	app.Use(metricsMiddleware())
	app.Use(fiberrecover.New())
	return app
}

func doReq(t *testing.T, app *fiber.App, method, target string) int {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, target, nil), fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestMetricsCollapsesUnmatchedPaths(t *testing.T) {
	resetHTTPMetrics()
	app := newMetricsApp()
	app.Get("/known", func(c fiber.Ctx) error { return c.SendString("ok") })

	for _, target := range []string{"/a", "/b", "/random/1", "/random/2?x=1"} {
		if code := doReq(t, app, http.MethodGet, target); code != http.StatusNotFound {
			t.Fatalf("%s: status %d", target, code)
		}
	}
	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", unmatchedRoute, "404")); got != 4 {
		t.Fatalf("unmatched total = %v, want 4", got)
	}
	if got := testutil.CollectAndCount(httpTotal); got != 1 {
		t.Fatalf("series = %d, want 1 (сырой путь утёк в label)", got)
	}
}

func TestMetricsUsesRouteTemplate(t *testing.T) {
	resetHTTPMetrics()
	app := newMetricsApp()
	app.Get("/albums/:artist/:album", func(c fiber.Ctx) error { return c.SendString("ok") })

	doReq(t, app, http.MethodGet, "/albums/secret-artist/secret-album")
	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", "/albums/:artist/:album", "200")); got != 1 {
		t.Fatalf("route template metric = %v, want 1", got)
	}
	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", "/albums/secret-artist/secret-album", "200")); got != 0 {
		t.Fatalf("сырой путь попал в label: %v", got)
	}
}

func TestMetricsPanicIs500(t *testing.T) {
	resetHTTPMetrics()
	app := newMetricsApp()
	app.Get("/boom", func(c fiber.Ctx) error { panic("boom") })

	if code := doReq(t, app, http.MethodGet, "/boom"); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", "/boom", "500")); got != 1 {
		t.Fatalf("panic metric = %v, want 1", got)
	}
	if got := testutil.ToFloat64(activeRequests); got != 0 {
		t.Fatalf("active requests = %v, want 0", got)
	}
}

func TestMetricsSkipsOwnScrape(t *testing.T) {
	resetHTTPMetrics()
	app := newMetricsApp()
	app.Get("/metrics", func(c fiber.Ctx) error { return c.SendString("") })

	doReq(t, app, http.MethodGet, "/metrics")
	if got := testutil.CollectAndCount(httpTotal); got != 0 {
		t.Fatalf("/metrics самонаблюдается: series = %d", got)
	}
}
