package platform

import (
	"runtime"
	"time"

	"github.com/gofiber/fiber/v3"
)

var procStart = time.Now()

// MetricFields возвращает runtime-метрики для /health.
func MetricFields() fiber.Map {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fiber.Map{
		"version":    "1.0.0",
		"uptime_sec": int(time.Since(procStart).Seconds()),
		"mem_mb":     m.Alloc / 1024 / 1024,
		"mem_sys_mb": m.Sys / 1024 / 1024,
		"goroutines": runtime.NumGoroutine(),
		"gc_count":   m.NumGC,
	}
}

// HealthJSON собирает ответ /health: runtime + дополнительные поля.
// Сначала накладывается extra, затем системные поля — иначе extra мог бы
// перетереть status/service или runtime-метрики.
func HealthJSON(service string, extra fiber.Map) fiber.Map {
	m := fiber.Map{}
	for k, v := range extra {
		m[k] = v
	}
	for k, v := range MetricFields() {
		m[k] = v
	}
	m["status"] = "ok"
	m["service"] = service
	return m
}
