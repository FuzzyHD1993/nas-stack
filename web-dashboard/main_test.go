package main

import (
	"encoding/hex"
	"os"
	"testing"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
)

func TestGetEnv(t *testing.T) {
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
	os.Unsetenv("__V101_REQUIRED__")
	if err := platform.ValidateEnv([]string{"__V101_REQUIRED__"}); err == nil {
		t.Error("ожидалась ошибка для отсутствующей переменной")
	}
	os.Setenv("__V101_REQUIRED__", "x")
	defer os.Unsetenv("__V101_REQUIRED__")
	if err := platform.ValidateEnv([]string{"__V101_REQUIRED__"}); err != nil {
		t.Errorf("неожиданная ошибка: %v", err)
	}
}

func TestGenerateSecret(t *testing.T) {
	s, err := generateSecret()
	if err != nil {
		t.Fatalf("generateSecret: %v", err)
	}
	if len(s) != 64 {
		t.Fatalf("secret length = %d, want 64 hex chars", len(s))
	}
	if s == "change-me-in-production" {
		t.Fatal("wizard must not fall back to the default JWT secret")
	}
	if _, err := hex.DecodeString(s); err != nil {
		t.Fatalf("secret is not hex: %v", err)
	}
}
