package main

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	httpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Длительность HTTP-запросов",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
		},
		[]string{"method", "path", "status"},
	)
	httpTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Количество HTTP-запросов",
		},
		[]string{"method", "path", "status"},
	)
	activeRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_active_requests",
			Help: "Активные HTTP-запросы",
		},
	)
)

// unmatchedRoute — единая метка для запросов вне роутов: сырой URL в label
// даёт неограниченную кардинальность и memory DoS.
const unmatchedRoute = "__unmatched__"

func init() {
	prometheus.MustRegister(httpDuration, httpTotal, activeRequests)
}

// metricsMiddleware собирает метрики запросов для Prometheus.
// c.Method()/c.Path() в Fiber v3 возвращают zero-copy строки над мутируемым
// буфером, поэтому для label-ов обязательно клонируем.
// Путь берём из шаблона роута, а не из сырого URL.
func metricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		// Служебные роуты не должны попадать в метрики трафика.
		if p := c.Path(); p == "/metrics" || p == "/health" {
			return c.Next()
		}

		method := strings.Clone(c.Method())
		start := time.Now()
		activeRequests.Inc()
		defer func() {
			activeRequests.Dec()

			path := unmatchedRoute
			if c.Matched() {
				if p := c.Route().Path; p != "" {
					path = p
				}
			}

			status := c.Response().StatusCode()
			if err != nil {
				var fe *fiber.Error
				if errors.As(err, &fe) {
					status = fe.Code
				} else {
					status = fiber.StatusInternalServerError
				}
			}

			labelStatus := strconv.Itoa(status)
			httpDuration.WithLabelValues(method, path, labelStatus).Observe(time.Since(start).Seconds())
			httpTotal.WithLabelValues(method, path, labelStatus).Inc()
		}()
		return c.Next()
	}
}
