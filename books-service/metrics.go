package main

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
)

// unmatchedRoute — единая метка для запросов вне таблицы роутов.
// Сырой путь в label использовать нельзя: клиент создаёт неограниченное
// число серий Prometheus (memory DoS).
const unmatchedRoute = "__unmatched__"

var (
	httpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Длительность HTTP-запросов",
			// Верхняя граница >= максимального таймаута makeClient (30s),
			// иначе медленный хвост не наблюдаем.
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 15, 30},
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

// normalizeMethod ограничивает кардинальность по method: Fiber отдаёт
// незарегистрированные методы как есть ("FOO1", "FOO2", ...).
func normalizeMethod(m string) string {
	switch strings.ToUpper(m) {
	case "GET":
		return "GET"
	case "HEAD":
		return "HEAD"
	case "POST":
		return "POST"
	case "PUT":
		return "PUT"
	case "PATCH":
		return "PATCH"
	case "DELETE":
		return "DELETE"
	case "OPTIONS":
		return "OPTIONS"
	case "CONNECT":
		return "CONNECT"
	case "TRACE":
		return "TRACE"
	default:
		return "OTHER"
	}
}

// metricsMiddleware собирает метрики запросов для Prometheus.
// Путь берётся из шаблона роута после c.Next() и только при c.Matched():
// у несматченного запроса c.Route().Path содержит сырой URL.
// Статус при err считается по fiber.Error: ErrorHandler вызывается уже
// после возврата из middleware.
func metricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		method := normalizeMethod(c.Method())
		start := time.Now()
		activeRequests.Inc()
		defer func() {
			activeRequests.Dec()

			path := unmatchedRoute
			if c.Matched() {
				if p := c.Route().Path; p != "" {
					path = strings.Clone(p)
				}
			}

			rec := recover()
			status := c.Response().StatusCode()
			if rec != nil {
				status = fiber.StatusInternalServerError
			} else if err != nil {
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

			if rec != nil {
				panic(rec)
			}
		}()
		return c.Next()
	}
}
