package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestGiteaBackupCommandKey(t *testing.T) {
	key := filepath.Join(t.TempDir(), "nas_ssh_key")
	if err := os.WriteFile(key, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NAS_SSH_KEY", key)
	t.Setenv("NAS_HOST", "nas.example")
	t.Setenv("NAS_USER", "backup")
	t.Setenv("NAS_SSH_PORT", "5022")
	t.Setenv("NAS_BACKUP_SCRIPT", "/tmp/backup.sh")
	t.Setenv("NAS_PASS", "")

	cmd, err := giteaBackupCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"-p 5022",
		"-i " + key,
		"IdentitiesOnly=yes",
		"BatchMode=yes",
		"backup@nas.example",
		"sh /tmp/backup.sh",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("command missing %q: %s", want, joined)
		}
	}
}

func TestGiteaBackupCommandPasswordFallback(t *testing.T) {
	t.Setenv("NAS_SSH_KEY", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("NAS_PASS", "test-password")
	t.Setenv("NAS_HOST", "nas.example")
	t.Setenv("NAS_USER", "backup")

	cmd, err := giteaBackupCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "sshpass") || !strings.Contains(joined, "-e ssh") {
		t.Errorf("expected sshpass -e ssh, got: %s", joined)
	}
	found := false
	for _, env := range cmd.Env {
		if env == "SSHPASS=test-password" {
			found = true
		}
	}
	if !found {
		t.Error("SSHPASS was not passed through the environment")
	}
	if strings.Contains(joined, "test-password") {
		t.Error("password must not appear in command arguments")
	}
}

func TestGiteaBackupCommandNoCredentials(t *testing.T) {
	t.Setenv("NAS_SSH_KEY", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("NAS_PASS", "")
	if _, err := giteaBackupCommand(context.Background()); err == nil {
		t.Error("expected error without key or password")
	}
}

func TestGiteaBackupTaskRegistered(t *testing.T) {
	for _, task := range tasks {
		if task.Name == "gitea-backup" {
			if task.Schedule != 24*time.Hour {
				t.Errorf("schedule = %v, want 24h", task.Schedule)
			}
			return
		}
	}
	t.Error("gitea-backup task is not registered")
}
