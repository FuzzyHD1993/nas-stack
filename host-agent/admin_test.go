package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

const testAdminToken = "TEST_ONLY_0123456789abcdef0123456789abcdef"

func adminTestEnv(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("REPO_DIR", dir)
	t.Setenv("ADMIN_TOKEN", token)
	return dir
}

func doReq(t *testing.T, app *fiber.App, method, target string, hdr map[string]string, body []byte) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Тест на РЕАЛЬНЫЙ роутер: удаление requireAdminToken в newApp() роняет этот тест.
func TestMutatingRoutesRequireAdminToken(t *testing.T) {
	adminTestEnv(t, testAdminToken)
	app := newApp()

	checked := 0
	for _, r := range app.GetRoutes(true) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}
		checked++
		key := r.Method + " " + r.Path

		if code, _ := doReq(t, app, r.Method, r.Path, nil, nil); code != fiber.StatusForbidden {
			t.Errorf("%s без токена = %d, want 403", key, code)
		}
		if code, _ := doReq(t, app, r.Method, r.Path, map[string]string{"X-Admin-Token": "wrong"}, nil); code != fiber.StatusForbidden {
			t.Errorf("%s с неверным токеном = %d, want 403", key, code)
		}
		if _, body := doReq(t, app, r.Method, r.Path, nil, nil); strings.Contains(body, testAdminToken) {
			t.Errorf("%s: токен эхоится в тело ответа", key)
		}
	}
	if checked == 0 {
		t.Fatal("не найдено ни одного мутирующего маршрута — проверьте GetRoutes")
	}
}

func TestReadOnlyRoutesRequireAdminToken(t *testing.T) {
	adminTestEnv(t, testAdminToken)
	app := newApp()

	for _, p := range []string{
		"/sensors", "/smart", "/disks", "/processes",
		"/network", "/uptime", "/load", "/gpu",
		"/cleanup/preview", "/update/status",
	} {
		if code, _ := doReq(t, app, http.MethodGet, p, nil, nil); code != fiber.StatusForbidden {
			t.Errorf("GET %s без токена = %d, want 403", p, code)
		}
	}

	if code, _ := doReq(t, app, http.MethodGet, "/health", nil, nil); code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", code)
	}
	if code, _ := doReq(t, app, http.MethodGet, "/metrics", nil, nil); code != http.StatusOK {
		t.Errorf("GET /metrics = %d, want 200", code)
	}
}

func TestAdminTokenMatrix(t *testing.T) {
	adminTestEnv(t, testAdminToken)
	app := newApp()

	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"нет заголовка", nil, fiber.StatusForbidden},
		{"пустой X-Admin-Token", map[string]string{"X-Admin-Token": ""}, fiber.StatusForbidden},
		{"неверный токен", map[string]string{"X-Admin-Token": "attacker"}, fiber.StatusForbidden},
		{"префикс токена", map[string]string{"X-Admin-Token": testAdminToken[:10]}, fiber.StatusForbidden},
		{"X-Gateway-Token не даёт прав", map[string]string{"X-Gateway-Token": testAdminToken}, fiber.StatusForbidden},
		{"токен в query", map[string]string{"X-Request-Id": testAdminToken}, fiber.StatusForbidden},
		{"верный X", map[string]string{"X-Admin-Token": testAdminToken}, fiber.StatusBadRequest},
		{"пробелы вокруг токена", map[string]string{"X-Admin-Token": "  " + testAdminToken + "  "}, fiber.StatusBadRequest},
		{"верный Bearer", map[string]string{"Authorization": "Bearer " + testAdminToken}, fiber.StatusBadRequest},
		{"bearer в нижнем регистре", map[string]string{"Authorization": "bearer " + testAdminToken}, fiber.StatusBadRequest},
		{"битый X + верный Bearer", map[string]string{
			"X-Admin-Token": "bad",
			"Authorization": "Bearer " + testAdminToken,
		}, fiber.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(t, app, http.MethodPost, "/duplicates/resolve", tc.hdr, nil)
			if code != tc.want {
				t.Fatalf("POST /duplicates/resolve = %d, want %d", code, tc.want)
			}
			if strings.Contains(body, testAdminToken) {
				t.Fatal("ответ эхо-тестит admin-токен")
			}
		})
	}
}

func TestAdminTokenFailsClosed(t *testing.T) {
	adminTestEnv(t, "")
	app := newApp()
	code, _ := doReq(t, app, http.MethodPost, "/update/run",
		map[string]string{"X-Admin-Token": "anything"}, nil)
	if code != fiber.StatusForbidden {
		t.Fatalf("без секрета = %d, want 403", code)
	}
}

func TestAdminTokenFromDotenv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REPO_DIR", dir)
	t.Setenv("ADMIN_TOKEN", "")
	env := "export ADMIN_TOKEN=" + testAdminToken + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	app := newApp()
	code, _ := doReq(t, app, http.MethodPost, "/duplicates/resolve",
		map[string]string{"X-Admin-Token": testAdminToken}, nil)
	if code != fiber.StatusBadRequest {
		t.Fatalf("токен из .env = %d, want 400 (тело пустое)", code)
	}
}

// --- /update/run: single-flight и отсутствие скрипта ---

func TestUpdateRunSingleFlight(t *testing.T) {
	dir := adminTestEnv(t, testAdminToken)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "update.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	prev := runUpdateScript
	runUpdateScript = func(string, string) ([]byte, error) {
		once.Do(func() { close(started) })
		<-release
		return nil, nil
	}
	t.Cleanup(func() { runUpdateScript = prev })
	updateRunning.Store(false)
	t.Cleanup(func() { updateRunning.Store(false) })

	app := newApp()
	hdr := map[string]string{"X-Admin-Token": testAdminToken}

	if code, _ := doReq(t, app, http.MethodPost, "/update/run", hdr, nil); code != http.StatusAccepted {
		t.Fatalf("первый POST = %d, want 202", code)
	}
	<-started
	if code, _ := doReq(t, app, http.MethodPost, "/update/run", hdr, nil); code != http.StatusConflict {
		t.Fatalf("параллельный POST = %d, want 409", code)
	}
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for updateRunning.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if updateRunning.Load() {
		t.Fatal("updateRunning не сброшен после завершения")
	}
}

func TestUpdateRunMissingScript(t *testing.T) {
	adminTestEnv(t, testAdminToken)
	app := newApp()
	code, _ := doReq(t, app, http.MethodPost, "/update/run",
		map[string]string{"X-Admin-Token": testAdminToken}, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("без update.sh = %d, want 500", code)
	}
}

// --- /duplicates/resolve: containment ---

func TestDuplicatesResolveRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "library")
	backup := filepath.Join(base, "backup")
	outside := filepath.Join(base, "outside")
	sibling := filepath.Join(base, "library-evil")
	for _, d := range []string{lib, backup, outside, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("ADMIN_TOKEN", testAdminToken)
	t.Setenv("REPO_DIR", t.TempDir())
	t.Setenv("LIBRARY_ROOT", lib)
	t.Setenv("DUPLICATES_BACKUP_DIR", backup)

	inside := filepath.Join(lib, "Movies", "keep.mkv")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}

	keepFile := filepath.Join(lib, "keep", "keep.mkv")
	if err := os.MkdirAll(filepath.Dir(keepFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	outsideFile := filepath.Join(outside, "secret.mkv")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	siblingFile := filepath.Join(sibling, "x.mkv")
	if err := os.WriteFile(siblingFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(lib, "link.mkv")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	payload, _ := json.Marshal(map[string]any{
		"paths": []string{
			outsideFile,
			siblingFile,
			link,
			inside,
		},
		"keep": keepFile,
	})

	code, body := doReq(t, newApp(), http.MethodPost, "/duplicates/resolve",
		map[string]string{"X-Admin-Token": testAdminToken}, payload)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, body)
	}

	if _, err := os.Stat(outsideFile); err != nil {
		t.Fatalf("файл вне библиотеки перемещён: %v", err)
	}
	if _, err := os.Stat(siblingFile); err != nil {
		t.Fatalf("файл у префиксного сиблинга перемещён: %v", err)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("файл внутри библиотеки не перемещён: %v", err)
	}

	var resp struct {
		Moved   []string `json:"moved"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Moved) != 1 || filepath.Base(resp.Moved[0]) != "keep.mkv" {
		t.Fatalf("moved = %v, want [keep.mkv]", resp.Moved)
	}
	if len(resp.Skipped) != 3 {
		t.Fatalf("skipped = %v, want 3 элемента", resp.Skipped)
	}
}

func TestResolveLibraryPath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	outside := filepath.Join(base, "outside")
	sibling := filepath.Join(base, "library-evil")
	for _, d := range []string{root, outside, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(root, "Movies", "ok.mkv")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.mkv")
	if err := os.WriteFile(secret, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	siblingFile := filepath.Join(sibling, "x.mkv")
	if err := os.WriteFile(siblingFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveLibraryPath(root, inside); err != nil {
		t.Fatalf("валидный путь отклонён: %v", err)
	}

	bad := []string{
		"",
		"relative.mkv",
		"/etc/passwd",
		filepath.Join(root, "..", "outside", "secret.mkv"),
		siblingFile,
		root,                          // сам корень
		filepath.Join(root, "Movies"), // каталог
	}
	for _, p := range bad {
		if got, err := resolveLibraryPath(root, p); err == nil {
			t.Errorf("resolveLibraryPath(%q) = %q, want error", p, got)
		}
	}

	link := filepath.Join(root, "link.mkv")
	if err := os.Symlink(secret, link); err == nil {
		if got, err := resolveLibraryPath(root, link); err == nil {
			t.Errorf("симлинк наружу принят: %q", got)
		}
	}
}

func TestReserveBackupTargetNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	first, err := reserveBackupTarget(dir, "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	second, err := reserveBackupTarget(dir, "movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("reserveBackupTarget вернул существующий файл")
	}
	for _, base := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := reserveBackupTarget(dir, base); err == nil {
			t.Errorf("reserveBackupTarget(%q) должен вернуть ошибку", base)
		}
	}
}

// --- stackEnvValue / parseDotenv ---

func TestStackEnvValuePrefersEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REPO_DIR", dir)
	t.Setenv("__AGENT_TEST_VAR__", "from-env")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("__AGENT_TEST_VAR__=from-dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stackEnvValue("__AGENT_TEST_VAR__", "def"); got != "from-env" {
		t.Fatalf("stackEnvValue = %q, want from-env", got)
	}
}

func TestStackEnvValueDotenvFormats(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REPO_DIR", dir)
	for _, k := range []string{
		"__AGENT_EXPORTED__", "__AGENT_QUOTED__", "__AGENT_INLINE__",
		"__AGENT_EQ__", "__AGENT_DUP__", "__AGENT_EMPTY__", "__AGENT_MISSING__",
	} {
		t.Setenv(k, "")
	}

	content := strings.Join([]string{
		"# comment",
		"export __AGENT_EXPORTED__=from-export",
		`__AGENT_QUOTED__="a=b # not comment"`,
		"__AGENT_INLINE__=value # trailing",
		"__AGENT_EQ__=a=b=c",
		"__AGENT_DUP__=first",
		"__AGENT_DUP__=second",
		"__AGENT_EMPTY__=",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"__AGENT_EXPORTED__": "from-export",
		"__AGENT_QUOTED__":   "a=b # not comment",
		"__AGENT_INLINE__":   "value",
		"__AGENT_EQ__":       "a=b=c",
		"__AGENT_DUP__":      "second",
	}
	for key, want := range cases {
		if got := stackEnvValue(key, "def"); got != want {
			t.Errorf("stackEnvValue(%q) = %q, want %q", key, got, want)
		}
	}
	if got := stackEnvValue("__AGENT_MISSING__", "def"); got != "def" {
		t.Errorf("missing = %q, want def", got)
	}
}

// --- agentBindAddrs ---

func TestAgentBindAddrsDefaultLoopback(t *testing.T) {
	t.Setenv("HOST_AGENT_BIND", "")
	t.Setenv("REPO_DIR", t.TempDir())
	if got := agentBindAddrs(); len(got) != 1 || got[0] != "127.0.0.1:9998" {
		t.Fatalf("agentBindAddrs = %v, want [127.0.0.1:9998]", got)
	}
}

func TestAgentBindAddrsDedupAndGarbage(t *testing.T) {
	t.Setenv("REPO_DIR", t.TempDir())
	t.Setenv("HOST_AGENT_BIND", "127.0.0.1:9998, 192.168.0.4:9998,127.0.0.1:9998, not-an-addr ,,")
	got := agentBindAddrs()
	want := []string{"127.0.0.1:9998", "192.168.0.4:9998"}
	if len(got) != len(want) {
		t.Fatalf("agentBindAddrs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("agentBindAddrs = %v, want %v", got, want)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:9998":   true,
		"localhost:9998":   true,
		"[::1]:9998":       true,
		"0.0.0.0:9998":     false,
		"192.168.0.4:9998": false,
		"garbage":          false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

// --- /cleanup/run: fail-closed ---

func TestCleanupRunRequiresExplicitTargets(t *testing.T) {
	adminTestEnv(t, testAdminToken)
	app := newApp()
	hdr := map[string]string{"X-Admin-Token": testAdminToken}

	for _, body := range [][]byte{
		nil,
		[]byte(`{`),
		[]byte(`{"targets":[]}`),
		[]byte(`{"targets":["../x"]}`),
		[]byte(`{"allow_kernels":true}`),
	} {
		code, _ := doReq(t, app, http.MethodPost, "/cleanup/run?dry=false", hdr, body)
		if code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, code)
		}
	}
}

func TestCleanupRunRejectsUnknownTarget(t *testing.T) {
	adminTestEnv(t, testAdminToken)
	app := newApp()
	hdr := map[string]string{"X-Admin-Token": testAdminToken}

	for _, target := range []string{"../x", "rm -rf /", "unknown", "podman_images;id", "old_kernels"} {
		body, _ := json.Marshal(map[string]any{"targets": []string{target}})
		code, _ := doReq(t, app, http.MethodPost, "/cleanup/run?dry=false", hdr, body)
		if code != http.StatusBadRequest {
			t.Errorf("target %q = %d, want 400", target, code)
		}
	}
}

func TestCleanupRunDryPreview(t *testing.T) {
	adminTestEnv(t, testAdminToken)
	app := newApp()
	code, _ := doReq(t, app, http.MethodPost, "/cleanup/run?dry=true",
		map[string]string{"X-Admin-Token": testAdminToken}, []byte(`{"all":true}`))
	if code != http.StatusOK {
		t.Fatalf("dry preview = %d, want 200", code)
	}
}
