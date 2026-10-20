package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
)

func metricLabels(t *testing.T, family string) []map[string]string {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []map[string]string
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			out = append(out, labels)
		}
	}
	return out
}

func doRequest(t *testing.T, app *fiber.App, method, path string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, path, nil))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	_ = resp.Body.Close()
}

func TestMetricsUseRouteTemplateAndRealStatus(t *testing.T) {
	app := fiber.New()
	app.Use(metricsMiddleware())
	app.Get("/read/:id", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	app.Get("/err", func(c fiber.Ctx) error { return fiber.ErrBadRequest })

	doRequest(t, app, http.MethodGet, "/read/secret-book-1")
	doRequest(t, app, http.MethodGet, "/read/secret-book-2")
	doRequest(t, app, http.MethodGet, "/junk-abc")
	doRequest(t, app, http.MethodGet, "/err")

	var sawTemplate, sawUnmatched404, sawBadRequest bool
	for _, l := range metricLabels(t, "http_requests_total") {
		p, st := l["path"], l["status"]
		if strings.Contains(p, "secret-book") || strings.Contains(p, "/junk-abc") {
			t.Fatalf("сырой путь попал в label path: %q", p)
		}
		switch p {
		case "/read/:id":
			if st == "200" {
				sawTemplate = true
			}
		case unmatchedRoute:
			if st == "404" {
				sawUnmatched404 = true
			}
		case "/err":
			if st == "400" {
				sawBadRequest = true
			}
		}
	}

	if !sawTemplate {
		t.Fatal("нет серии path=/read/:id status=200")
	}
	if !sawUnmatched404 {
		t.Fatalf("несматченный запрос учтён не как 404 (path=%s)", unmatchedRoute)
	}
	if !sawBadRequest {
		t.Fatal("fiber.ErrBadRequest учтён не как 400")
	}
}

func TestMetricsNormalizesMethod(t *testing.T) {
	if got := normalizeMethod("x-junk-1"); got != "OTHER" {
		t.Fatalf("normalizeMethod = %q, want OTHER", got)
	}
	for _, m := range []string{"GET", "POST", "HEAD", "PATCH", "OPTIONS"} {
		if got := normalizeMethod(strings.ToLower(m)); got != m {
			t.Fatalf("normalizeMethod(%q) = %q, want %q", m, got, m)
		}
	}
}
