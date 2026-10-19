package main

import (
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

func init() {
	prometheus.MustRegister(httpDuration, httpTotal, activeRequests)
}

// metricsMiddleware собирает метрики запросов для Prometheus.
// c.Method()/c.Path() в Fiber v3 возвращают zero-copy строки над мутируемым
// буфером, поэтому для label-ов обязательно клонируем (иначе порча метрик).
func metricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		method := strings.Clone(c.Method())
		path := strings.Clone(c.Path())
		start := time.Now()
		activeRequests.Inc()
		defer activeRequests.Dec()
		err := c.Next()
		duration := time.Since(start).Seconds()
		status := strconv.Itoa(c.Response().StatusCode())
		httpDuration.WithLabelValues(method, path, status).Observe(duration)
		httpTotal.WithLabelValues(method, path, status).Inc()
		return err
	}
}
