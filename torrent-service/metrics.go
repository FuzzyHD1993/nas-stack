package main

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
)

// unmatchedRoute — единая метка для запросов вне роутов: сырой URL в label
// даёт неограниченное число серий Prometheus.
const unmatchedRoute = "__unmatched__"

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
// c.Method() в Fiber v3 возвращает zero-copy строку над мутируемым буфером,
// поэтому method клонируем. Путь берём из шаблона роута, для несматченных
// запросов — sentinel (иначе любой сканер создаёт неограниченное число серий).
// Статус вычисляем после c.Next() с учётом err: ErrorHandler вызывается уже
// после возврата из middleware, поэтому Response().StatusCode() для ошибок
// ещё не финальный.
func metricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		method := strings.Clone(c.Method())
		start := time.Now()
		activeRequests.Inc()

		defer func() {
			activeRequests.Dec()

			path := unmatchedRoute
			if c.Matched() {
				if r := c.Route(); r != nil && r.Path != "" {
					path = strings.Clone(r.Path)
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
