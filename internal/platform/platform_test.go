package platform

import (
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestGetEnv(t *testing.T) {
	if got := GetEnv("__NAS_TEST_MISSING__", "def"); got != "def" {
		t.Fatalf("GetEnv default = %q, want %q", got, "def")
	}
}

func TestHealthJSON(t *testing.T) {
	m := HealthJSON("test-service", fiber.Map{"echo": "hi"})
	if m["status"] != "ok" {
		t.Fatalf("status = %v, want ok", m["status"])
	}
	if m["service"] != "test-service" {
		t.Fatalf("service = %v", m["service"])
	}
	for _, k := range []string{"goroutines", "mem_mb", "uptime_sec", "gc_count"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing metric %q", k)
		}
	}
	if m["echo"] != "hi" {
		t.Fatalf("extra not merged: %v", m["echo"])
	}
}

func TestValidateEnv(t *testing.T) {
	t.Setenv("__V101_REQUIRED__", "")
	if err := ValidateEnv([]string{"__V101_REQUIRED__"}); err == nil {
		t.Error("ожидалась ошибка для отсутствующей переменной")
	}
	t.Setenv("__V101_REQUIRED__", "   ")
	if err := ValidateEnv([]string{"__V101_REQUIRED__"}); err == nil {
		t.Error("пробельное значение должно считаться отсутствующим")
	}
	t.Setenv("__V101_REQUIRED__", "x")
	if err := ValidateEnv([]string{"__V101_REQUIRED__"}); err != nil {
		t.Errorf("неожиданная ошибка: %v", err)
	}
}

func TestHealthJSONReservedFieldsAuthoritative(t *testing.T) {
	m := HealthJSON("svc", fiber.Map{
		"status":     "degraded",
		"service":    "evil",
		"goroutines": -1,
	})
	if m["status"] != "ok" || m["service"] != "svc" {
		t.Fatalf("extra перетёр reserved-поля: %#v", m)
	}
	if v, ok := m["goroutines"].(int); !ok || v < 0 {
		t.Fatalf("extra перетёр runtime-метрику: %#v", m["goroutines"])
	}
}
