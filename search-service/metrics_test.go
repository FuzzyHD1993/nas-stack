package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func resetHTTPMetrics() {
	httpDuration.Reset()
	httpTotal.Reset()
	activeRequests.Set(0)
}

func doMetricsReq(t *testing.T, app *fiber.App, method, target string) int {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, target, nil), fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestMetricsCollapsesUnmatchedPaths(t *testing.T) {
	resetHTTPMetrics()
	app := newFiber()
	app.Get("/known", func(c fiber.Ctx) error { return c.SendString("ok") })

	for _, target := range []string{"/a", "/b", "/random/1", "/random/2"} {
		if code := doMetricsReq(t, app, http.MethodGet, target); code != http.StatusNotFound {
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

func TestMetricsNormalizesMethod(t *testing.T) {
	if got := metricMethod("FOOBAR"); got != "OTHER" {
		t.Fatalf("metricMethod(FOOBAR) = %q, want OTHER", got)
	}
	if got := metricMethod("get"); got != "GET" {
		t.Fatalf("metricMethod(get) = %q, want GET", got)
	}
	if got := metricStatus(99); got != "unknown" {
		t.Fatalf("metricStatus(99) = %q, want unknown", got)
	}
}

func TestMetricsPanicIs500(t *testing.T) {
	resetHTTPMetrics()
	app := newFiber()
	app.Get("/boom", func(c fiber.Ctx) error { panic("boom") })

	if code := doMetricsReq(t, app, http.MethodGet, "/boom"); code != http.StatusInternalServerError {
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
	app := newFiber()
	app.Get("/metrics", func(c fiber.Ctx) error { return c.SendString("") })

	doMetricsReq(t, app, http.MethodGet, "/metrics")
	if got := testutil.CollectAndCount(httpTotal); got != 0 {
		t.Fatalf("/metrics самонаблюдается: series = %d", got)
	}
}
