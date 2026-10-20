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

var (
	httpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_request_duration_seconds",
			Help: "Длительность HTTP-запросов",
			// proxy timeout = 30s, /stats/* = 60s: иначе хвост уходит в +Inf.
			Buckets: []float64{
				0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
				1, 2.5, 5, 10, 30, 60, 120,
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

// metricsRegistry — собственный реестр. Default registry не используем:
// иначе в экспозицию попадают go_*/process_* и любой чужой пакет с тем же
// именем метрики роняет процесс через MustRegister.
var metricsRegistry = prometheus.NewRegistry()

func init() {
	metricsRegistry.MustRegister(httpDuration, httpTotal, activeRequests)
}

// unmatchedRoute — единая метка для запросов, не попавших ни в один роут.
// Сырой путь в label использовать нельзя: иначе любой клиент создаёт
// неограниченное число серий Prometheus (memory DoS).
const unmatchedRoute = "__unmatched__"

// clientClosedRequest — нестандартный код (nginx), которым помечают обрыв
// запроса клиентом: http.StatusText для него нет.
const clientClosedRequest = 499

// maxRouteLabelLen — защита от патологической длины шаблона роута.
const maxRouteLabelLen = 256

// metricMethods — allowlist методов. Произвольную строку клиента в label
// не пускаем: иначе любой METHOD создаёт новую серию Prometheus.
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
// (401/403 от app.Use) c.Matched() == false, но c.IsMiddleware() == true —
// тогда метка = префикс middleware, что позволяет отделить всплеск 401
// от сканирования несуществующих путей.
func metricPath(c fiber.Ctx) string {
	if !c.Matched() && !c.IsMiddleware() {
		return unmatchedRoute
	}
	p := c.Route().Path
	if p == "" || p == "/" || len(p) > maxRouteLabelLen {
		return unmatchedRoute
	}
	return p
}

func recordRequest(method, path string, status int, d time.Duration) {
	labelStatus := metricStatus(status)
	httpDuration.WithLabelValues(method, path, labelStatus).Observe(d.Seconds())
	httpTotal.WithLabelValues(method, path, labelStatus).Inc()
}

// metricsHandler отдаёт только метрики gateway, без go_*/process_*.
func metricsHandler() http.Handler {
	return promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{})
}

// metricsMiddleware собирает метрики запросов для Prometheus.
// Должен регистрироваться ДО recover: тогда паника приходит как err и
// учитывается как 500. Дополнительно defer сам ловит панику (defense in depth).
func metricsMiddleware() fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		// c.Method() в Fiber v3 возвращает zero-copy строку над мутируемым
		// буфером, поэтому method клонируем до c.Next().
		method := metricMethod(strings.Clone(c.Method()))
		start := time.Now()
		activeRequests.Inc()

		defer func() {
			activeRequests.Dec()
			if r := recover(); r != nil {
				// Паника: клиент получит 500/обрыв, но не 200 в метриках.
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
