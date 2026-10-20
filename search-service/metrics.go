package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// unmatchedRoute — единая метка для запросов вне роутов: сырой URL в label
	// даёт неограниченное число серий Prometheus.
	unmatchedRoute = "__unmatched__"
	// clientClosedRequest — nginx-код обрыва запроса клиентом.
	clientClosedRequest = 499
	// maxRouteLabelLen — защита от патологической длины шаблона роута.
	maxRouteLabelLen = 256
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

// Собственный реестр: Default registry экспортирует go_*/process_* и роняет
// процесс через MustRegister при коллизии имён.
var metricsRegistry = prometheus.NewRegistry()

func init() {
	metricsRegistry.MustRegister(httpDuration, httpTotal, activeRequests)
}

// Произвольный токен метода клиента в label не пускаем.
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

// metricPath берёт шаблон роута, а не URL. Для коротких ответов middleware
// (401/403 от app.Use) Matched == false, но IsMiddleware == true.
func metricPath(c fiber.Ctx) string {
	if !c.Matched() && !c.IsMiddleware() {
		return unmatchedRoute
	}
	r := c.Route()
	if r == nil {
		return unmatchedRoute
	}
	p := r.Path
	if p == "" || p == "/" || len(p) > maxRouteLabelLen {
		return unmatchedRoute
	}
	return p
}

func recordRequest(method, path string, status int, d time.Duration) {
	label := metricStatus(status)
	httpDuration.WithLabelValues(method, path, label).Observe(d.Seconds())
	httpTotal.WithLabelValues(method, path, label).Inc()
}

// metricsHandler отдаёт только метрики сервиса, без go_*/process_*.
func metricsHandler() http.Handler {
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{})
}

// metricsMiddleware собирает метрики запросов. Регистрируется ДО
// recover.New(): тогда паника приходит как err и учитывается как 500.
// recover() в defer — defense in depth.
func metricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		// Скрейп не наблюдаем: иначе /metrics измеряет сам себя.
		if c.Path() == "/metrics" {
			return c.Next()
		}

		// c.Method() в Fiber v3 возвращает zero-copy строку над мутируемым
		// буфером, поэтому method клонируем.
		method := metricMethod(strings.Clone(c.Method()))
		start := time.Now()
		activeRequests.Inc()

		defer func() {
			activeRequests.Dec()
			if r := recover(); r != nil {
				// Клиент получит 500/обрыв, но не 200 в метриках.
				recordRequest(method, metricPath(c), fiber.StatusInternalServerError, time.Since(start))
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
			recordRequest(method, metricPath(c), status, time.Since(start))
		}()

		return c.Next()
	}
}
