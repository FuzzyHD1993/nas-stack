package main

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var errMetricsBoom = errors.New("boom")

func TestMetricsMiddlewareStatusFromError(t *testing.T) {
	app := fiber.New()
	app.Use(metricsMiddleware())
	app.Get("/metrics-test/teapot", func(c fiber.Ctx) error { return fiber.ErrTeapot })
	app.Get("/metrics-test/boom", func(c fiber.Ctx) error { return errMetricsBoom })

	cases := []struct{ url, status string }{
		{"/metrics-test/teapot", "418"},
		{"/metrics-test/boom", "500"},
	}
	for _, tc := range cases {
		before := testutil.ToFloat64(httpTotal.WithLabelValues("GET", tc.url, tc.status))
		resp, err := app.Test(httptest.NewRequest("GET", tc.url, nil))
		if err != nil {
			t.Fatal(err)
		}
		after := testutil.ToFloat64(httpTotal.WithLabelValues("GET", tc.url, tc.status))
		if after-before != 1 {
			t.Errorf("%s: status=%d, metric delta=%v", tc.url, resp.StatusCode, after-before)
		}
	}
}

func TestMetricsMiddlewareUsesRouteTemplate(t *testing.T) {
	app := fiber.New()
	app.Use(metricsMiddleware())
	app.Get("/metrics-test/movies/:id", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	const route = "/metrics-test/movies/:id"
	before := testutil.ToFloat64(httpTotal.WithLabelValues("GET", route, "200"))
	for _, u := range []string{"/metrics-test/movies/1", "/metrics-test/movies/2"} {
		if _, err := app.Test(httptest.NewRequest("GET", u, nil)); err != nil {
			t.Fatal(err)
		}
	}
	after := testutil.ToFloat64(httpTotal.WithLabelValues("GET", route, "200"))
	if after-before != 2 {
		t.Fatalf("route template delta = %v, want 2", after-before)
	}
	if got := testutil.ToFloat64(httpTotal.WithLabelValues("GET", "/metrics-test/movies/1", "200")); got != 0 {
		t.Fatalf("raw path leaked into label: %v", got)
	}
}
