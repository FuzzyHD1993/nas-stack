package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	httpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Длительность HTTP-запросов",
			// /events и /stats/* ходят в БД: без верхних бакетов хвост уходит в +Inf.
			Buckets: []float64{
				0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
				1, 2.5, 5, 10, 30, 60,
			},
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

// metricsRegistry — собственный реестр: default registry экспонирует go_*/process_*
// (точная версия Go, RSS, FDs) и падает в MustRegister при конфликте имён.
var metricsRegistry = prometheus.NewRegistry()

func init() {
	metricsRegistry.MustRegister(httpDuration, httpTotal, activeRequests)
}

// metricsHandler отдаёт только метрики history-service.
func metricsHandler() http.Handler {
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{})
}

const (
	// unmatchedRoute — единая метка для запросов, не попавших ни в один роут.
	// Сырой путь в label использовать нельзя: любой клиент создаёт
	// неограниченное число серий Prometheus (memory DoS).
	unmatchedRoute = "__unmatched__"
	// maxRouteLabelLen — защита от патологической длины шаблона роута.
	maxRouteLabelLen = 256
	// clientClosedRequest — нестандартный код nginx для оборванного запроса.
	clientClosedRequest = 499
)

// metricMethods — allowlist методов: произвольная строка клиента не должна
// создавать новую серию.
var metricMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {},
	"DELETE": {}, "OPTIONS": {}, "CONNECT": {}, "TRACE": {}, "QUERY": {},
}

func metricMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	if _, ok := metricMethods[m]; ok {
		return m
	}
	return "OTHER"
}

func metricStatus(code int) string {
	if code < 100 || code > 599 {
		return "unknown"
	}
	return strconv.Itoa(code)
}

// metricPath берёт шаблон роута, а не URL. Для 401/403 от app.Use
// c.Matched() == false, но c.IsMiddleware() == true — метка = префикс
// middleware, чтобы отличать всплеск 401 от сканирования путей.
func metricPath(c fiber.Ctx) string {
	if !c.Matched() && !c.IsMiddleware() {
		return unmatchedRoute
	}
	p := c.Route().Path
	if p == "" || p == "/" || len(p) > maxRouteLabelLen || !utf8.ValidString(p) {
		return unmatchedRoute
	}
	return strings.Clone(p)
}

func recordRequest(method, path string, status int, d time.Duration) {
	labelStatus := metricStatus(status)
	httpDuration.WithLabelValues(method, path, labelStatus).Observe(d.Seconds())
	httpTotal.WithLabelValues(method, path, labelStatus).Inc()
}

// metricsMiddleware собирает метрики запросов для Prometheus.
// Регистрируется ДО recover: паника приходит сюда как err и учитывается как 500.
// Дополнительно defer сам ловит панику (defense in depth).
func metricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		method := metricMethod(strings.Clone(c.Method()))
		start := time.Now()
		activeRequests.Inc()

		defer func() {
			activeRequests.Dec()
			path := metricPath(c)

			if r := recover(); r != nil {
				// Паника: клиент получит 500/обрыв, но не 200 в метриках.
				recordRequest(method, path, fiber.StatusInternalServerError, time.Since(start))
				panic(r)
			}

			status := c.Response().StatusCode()
			if err != nil {
				var fe *fiber.Error
				switch {
				case errors.As(err, &fe):
					status = fe.Code
				case errors.Is(err, context.Canceled):
					status = clientClosedRequest
				default:
					status = fiber.StatusInternalServerError
				}
			}
			recordRequest(method, path, status, time.Since(start))
		}()

		return c.Next()
	}
}
