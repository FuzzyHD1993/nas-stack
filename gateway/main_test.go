package main

import (
	"testing"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
)

func TestGetEnv(t *testing.T) {
	// t.Setenv автоматически восстанавливает исходное значение и делает
	// мутацию env безопасной для параллельных тестов пакета.
	t.Setenv("__NAS_TEST_MISSING__", "")
	if got := platform.GetEnv("__NAS_TEST_MISSING__", "def"); got != "def" {
		t.Fatalf("getEnv default = %q, want %q", got, "def")
	}
}

func TestHealthJSON(t *testing.T) {
	m := platform.HealthJSON("test-service", fiber.Map{"echo": "hi"})
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
	if err := platform.ValidateEnv([]string{"__V101_REQUIRED__"}); err == nil {
		t.Error("ожидалась ошибка для отсутствующей переменной")
	}
	t.Setenv("__V101_REQUIRED__", "x")
	if err := platform.ValidateEnv([]string{"__V101_REQUIRED__"}); err != nil {
		t.Errorf("неожиданная ошибка: %v", err)
	}
}
