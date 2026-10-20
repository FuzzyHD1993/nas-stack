package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// /tasks не должен раскрывать командные строки (пути/аргументы хоста).
func TestTasksDoNotExposeCommands(t *testing.T) {
	app := newFiber()
	registerRoutes(app)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/tasks", nil), fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var got []map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, body)
	}
	if strings.Contains(string(body), `"command"`) {
		t.Fatalf("/tasks раскрывает command: %s", body)
	}
	for _, task := range got {
		if _, ok := task["name"]; !ok {
			t.Fatalf("task без name: %v", task)
		}
		if _, ok := task["schedule"]; !ok {
			t.Fatalf("task без schedule: %v", task)
		}
	}
}

func TestTasksHaveRunners(t *testing.T) {
	seen := map[string]bool{}
	for _, task := range tasks {
		if seen[task.Name] {
			t.Fatalf("дубль задачи %q", task.Name)
		}
		seen[task.Name] = true
		if task.Command == "" {
			if _, ok := taskHandlers[task.Name]; !ok {
				t.Errorf("у задачи %q нет runner'а", task.Name)
			}
		}
	}
	for name := range taskHandlers {
		if !seen[name] {
			t.Errorf("runner %q не зарегистрирован в tasks", name)
		}
	}
}

func TestGiteaBackupCommandRejectsInjection(t *testing.T) {
	cases := []struct {
		name, host, user, port, script string
	}{
		{"host-semicolon", "nas.example; touch /tmp/pwned", "backup", "5022", "/tmp/backup.sh"},
		{"host-option", "-oProxyCommand=evil", "backup", "5022", "/tmp/backup.sh"},
		{"user-semicolon", "nas.example", "backup;id", "5022", "/tmp/backup.sh"},
		{"port-injection", "nas.example", "backup", "5022;id", "/tmp/backup.sh"},
		{"script-semicolon", "nas.example", "backup", "5022", "/tmp/backup.sh;id"},
		{"script-relative", "nas.example", "backup", "5022", "backup.sh"},
		{"script-dotdot", "nas.example", "backup", "5022", "/tmp/../etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NAS_SSH_KEY", filepath.Join(t.TempDir(), "missing"))
			t.Setenv("NAS_PASS", "x")
			t.Setenv("NAS_HOST", tc.host)
			t.Setenv("NAS_USER", tc.user)
			t.Setenv("NAS_SSH_PORT", tc.port)
			t.Setenv("NAS_BACKUP_SCRIPT", tc.script)
			if _, err := giteaBackupCommand(context.Background()); err == nil {
				t.Fatalf("враждебные параметры приняты: %+v", tc)
			}
		})
	}
}

func TestGiteaBackupCommandUsesPersistentKnownHosts(t *testing.T) {
	key := filepath.Join(t.TempDir(), "nas_ssh_key")
	if err := os.WriteFile(key, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NAS_SSH_KEY", key)
	t.Setenv("NAS_HOST", "nas.example")
	t.Setenv("NAS_USER", "backup")
	t.Setenv("NAS_STRICT_HOST_KEY", "yes")
	t.Setenv("NAS_KNOWN_HOSTS", "/root/.ssh/known_hosts")

	cmd, err := giteaBackupCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if strings.Contains(args, "/tmp/gitea_known_hosts") {
		t.Fatalf("known_hosts должен переживать рестарт: %s", args)
	}
	for _, want := range []string{"StrictHostKeyChecking=yes", "UserKnownHostsFile=/root/.ssh/known_hosts"} {
		if !strings.Contains(args, want) {
			t.Errorf("command missing %q: %s", want, args)
		}
	}
}

func TestListBackupsUsesRelativePaths(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BACKUP_DIR", base)
	if err := os.MkdirAll(filepath.Join(base, "daily"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "daily", "nas_history-1.sql.gz"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := listBackups()
	daily, ok := m["daily"].([]fiber.Map)
	if !ok || len(daily) != 1 {
		t.Fatalf("daily = %#v", m["daily"])
	}
	file, _ := daily[0]["file"].(string)
	if filepath.IsAbs(file) || strings.Contains(file, base) {
		t.Fatalf("абсолютный путь утёк в API: %q", file)
	}
	if file != "daily/nas_history-1.sql.gz" {
		t.Fatalf("file = %q", file)
	}
}

func TestSafeBackupPathRelative(t *testing.T) {
	base := t.TempDir()
	t.Setenv("BACKUP_DIR", base)
	if err := os.MkdirAll(filepath.Join(base, "daily"), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "daily", "nas_history-1.sql.gz")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := safeBackupPath("daily/nas_history-1.sql.gz")
	if err != nil || got != want {
		t.Fatalf("safeBackupPath = %q, %v; want %q", got, err, want)
	}
	if _, err := safeBackupPath("../etc/passwd"); err == nil {
		t.Fatal("относительный path traversal принят")
	}
}
