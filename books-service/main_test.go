package main

import (
	"strings"
	"testing"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
)

func TestGetEnv(t *testing.T) {
	t.Setenv("__NAS_TEST_PRESENT__", "set")
	if got := platform.GetEnv("__NAS_TEST_PRESENT__", "def"); got != "set" {
		t.Fatalf("GetEnv = %q, want %q", got, "set")
	}
	if got := platform.GetEnv("__NAS_TEST_MISSING_"+t.Name(), "def"); got != "def" {
		t.Fatalf("GetEnv default = %q, want %q", got, "def")
	}
}

func TestHealthJSON(t *testing.T) {
	m := platform.HealthJSON("test-service", fiber.Map{
		"echo":       "hi",
		"status":     "degraded",
		"service":    "evil",
		"goroutines": -1,
	})

	if m["status"] != "ok" {
		t.Fatalf("status = %v, want ok", m["status"])
	}
	if m["service"] != "test-service" {
		t.Fatalf("service = %v, want test-service", m["service"])
	}
	if m["echo"] != "hi" {
		t.Fatalf("extra not merged: %v", m["echo"])
	}
	for _, k := range []string{"goroutines", "mem_mb", "uptime_sec", "gc_count"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing metric %q", k)
		}
	}
	if g, ok := m["goroutines"].(int); !ok || g < 0 {
		t.Fatalf("extra перетёр runtime-метрику: %#v", m["goroutines"])
	}
}

func TestValidateEnv(t *testing.T) {
	const key = "__V101_REQUIRED__"

	t.Setenv(key, "")
	err := platform.ValidateEnv([]string{key})
	if err == nil {
		t.Fatal("ожидалась ошибка для пустой переменной")
	}
	if !strings.Contains(err.Error(), key) {
		t.Fatalf("в ошибке нет имени переменной: %v", err)
	}

	t.Setenv(key, "   ")
	if err := platform.ValidateEnv([]string{key}); err == nil {
		t.Fatal("пробельное значение должно считаться отсутствующим")
	}

	t.Setenv(key, "x")
	if err := platform.ValidateEnv([]string{key}); err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
}
