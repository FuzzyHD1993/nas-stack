package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

func TestOOMCheckRejectsInjection(t *testing.T) {
	t.Setenv("OOM_CHECK_MINUTES", `20"; touch /tmp/pwn; #`)
	app := fiber.New()
	app.Get("/oom-check", handleOOMCheck)

	resp, err := app.Test(httptest.NewRequest("GET", "/oom-check", nil), fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSetEnvValueAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")

	initial := "A=1\nSECRET_X=old\nSECRET_X=dup\n"
	if err := os.WriteFile(p, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setEnvValue(p, "SECRET_X", "new"); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "SECRET_X=") != 1 || !strings.Contains(string(b), "SECRET_X=new") {
		t.Fatalf(".env повреждён: %q", b)
	}
	if !strings.Contains(string(b), "A=1") {
		t.Fatalf("потерян другой ключ: %q", b)
	}

	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode = %o, want 600", st.Mode().Perm())
	}

	bak, err := os.ReadFile(p + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != initial {
		t.Fatalf("backup = %q, want %q", bak, initial)
	}
}

func TestSetEnvValueCollapsesDuplicates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("export A=1\nA=2\nB=3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setEnvValue(p, "A", "9"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "A="); n != 1 {
		t.Fatalf("A встречается %d раз: %q", n, b)
	}
	if !strings.Contains(string(b), "A=9") || !strings.Contains(string(b), "B=3") {
		t.Fatalf("значение не обновлено: %q", b)
	}
}

func TestSetEnvValueDoesNotWipeOnReadError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := setEnvValue(p, "A", "1"); err == nil {
		t.Fatal("ожидалась ошибка чтения каталога вместо .env")
	}
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		t.Fatalf(".env каталог повреждён: %v", err)
	}
}

func TestValidEnvValueRejectsDotenvBreakers(t *testing.T) {
	for _, v := range []string{"a\"b", "a'b", "a#b", "a$b", "a\nb", "a`b", "a\\b", "***", " a", strings.Repeat("x", 5000)} {
		if validEnvValue(v) {
			t.Errorf("validEnvValue(%q) = true, want false", v)
		}
	}
	for _, v := range []string{"", "normal", "abc-123_./=+", "/mnt/nas/root", "Привет", "postgres://nas:secret@db:5432/nas"} {
		if !validEnvValue(v) {
			t.Errorf("validEnvValue(%q) = false, want true", v)
		}
	}
}

func TestTokensEqual(t *testing.T) {
	if !tokensEqual("abc", "abc") {
		t.Fatal("одинаковые токены должны совпадать")
	}
	if tokensEqual("abc", "abd") || tokensEqual("abc", "abcd") {
		t.Fatal("разные токены не должны совпадать")
	}
}

func TestSendNtfyRejectsTopicTraversal(t *testing.T) {
	t.Setenv("NTFY_URL", "http://127.0.0.1:1")
	t.Setenv("NTFY_TOPIC", "../admin")
	if err := sendNtfy(context.Background(), "t", "b", "info", "x"); err == nil {
		t.Fatal("ожидалась ошибка для NTFY_TOPIC=../admin")
	}
}

func TestSendNtfyDoesNotFollowRedirect(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()

	t.Setenv("NTFY_URL", srv.URL)
	t.Setenv("NTFY_TOPIC", "nas-alerts")

	if err := sendNtfy(context.Background(), "t", "b", "info", "x"); err == nil {
		t.Fatal("редирект должен возвращать ошибку, а не выполняться")
	}
	if !hit {
		t.Fatal("запрос не дошёл до тестового сервера")
	}
}

func TestLoopbackHostAndOrigin(t *testing.T) {
	for _, h := range []string{"127.0.0.1:8083", "127.0.0.1", "[::1]:8083", "localhost:8083", "127.0.0.2:8083"} {
		if !loopbackHost(h) {
			t.Errorf("loopbackHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"", "evil.com", "evil.com:8083", "10.0.0.1"} {
		if loopbackHost(h) {
			t.Errorf("loopbackHost(%q) = true, want false", h)
		}
	}

	if !loopbackOrigin("") || !loopbackOrigin("http://127.0.0.1:8083") {
		t.Fatal("loopback Origin должен приниматься")
	}
	if loopbackOrigin("http://evil.com") || loopbackOrigin("null") {
		t.Fatal("внешний Origin должен отклоняться")
	}
}

func TestParseEnvQuotesAndComments(t *testing.T) {
	vars := parseEnv("export A=1\nB=\"two words\"\nC=three # comment\n# skip\n")
	got := make(map[string]string, len(vars))
	for _, kv := range vars {
		got[kv.Key] = kv.Value
	}
	if got["A"] != "1" || got["B"] != "two words" || got["C"] != "three" {
		t.Fatalf("parseEnv = %v", got)
	}
}
