package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
)

func newAuthTestApp() *fiber.App {
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		switch c.Path() {
		case "/health", "/metrics":
			return c.Next()
		}
		return requireGatewayToken(c)
	})
	app.Get("/health", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/metrics", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/events", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Delete("/events", requireAdminToken, func(c fiber.Ctx) error { return c.SendString("deleted") })
	return app
}

func doTest(t *testing.T, app *fiber.App, method, target string, headers map[string]string) int {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("%s %s: app.Test: %v", method, target, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestTokenEqual(t *testing.T) {
	if !tokenEqual("secret", "secret") {
		t.Fatal("одинаковые токены должны совпадать")
	}
	if tokenEqual("secret", "other") {
		t.Fatal("разные токены не должны совпадать")
	}
	if tokenEqual("", "") {
		t.Fatal("пустые токены не должны совпадать")
	}
}

func TestGatewayTokenMiddleware(t *testing.T) {
	prevG, prevA := gatewayToken, adminToken
	t.Cleanup(func() { gatewayToken, adminToken = prevG, prevA })
	gatewayToken, adminToken = "gw-secret", "admin-secret"

	app := newAuthTestApp()

	if got := doTest(t, app, http.MethodGet, "/health", nil); got != http.StatusOK {
		t.Fatalf("/health = %d, want 200", got)
	}
	if got := doTest(t, app, http.MethodGet, "/metrics", nil); got != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", got)
	}
	if got := doTest(t, app, http.MethodGet, "/events", nil); got != http.StatusUnauthorized {
		t.Fatalf("/events без токена = %d, want 401", got)
	}
	if got := doTest(t, app, http.MethodGet, "/events", map[string]string{
		"X-Gateway-Token": "wrong",
	}); got != http.StatusUnauthorized {
		t.Fatalf("/events с неверным токеном = %d, want 401", got)
	}
	if got := doTest(t, app, http.MethodGet, "/events", map[string]string{
		"X-Gateway-Token": "gw-secret",
	}); got != http.StatusOK {
		t.Fatalf("/events с токеном = %d, want 200", got)
	}

	// Пустой токен = fail-closed.
	gatewayToken = ""
	if got := doTest(t, app, http.MethodGet, "/events", nil); got != http.StatusServiceUnavailable {
		t.Fatalf("/events без настроенного токена = %d, want 503", got)
	}
}

func TestAdminTokenMiddleware(t *testing.T) {
	prevG, prevA := gatewayToken, adminToken
	t.Cleanup(func() { gatewayToken, adminToken = prevG, prevA })
	gatewayToken, adminToken = "gw-secret", "admin-secret"

	app := newAuthTestApp()

	if got := doTest(t, app, http.MethodDelete, "/events?all=1", map[string]string{
		"X-Gateway-Token": "gw-secret",
	}); got != http.StatusUnauthorized {
		t.Fatalf("DELETE без admin-токена = %d, want 401", got)
	}
	if got := doTest(t, app, http.MethodDelete, "/events?all=1", map[string]string{
		"X-Gateway-Token": "gw-secret",
		"X-Admin-Token":   "wrong",
	}); got != http.StatusUnauthorized {
		t.Fatalf("DELETE с неверным admin-токеном = %d, want 401", got)
	}
	if got := doTest(t, app, http.MethodDelete, "/events?all=1", map[string]string{
		"X-Gateway-Token": "gw-secret",
		"X-Admin-Token":   "admin-secret",
	}); got != http.StatusOK {
		t.Fatalf("DELETE с admin-токеном = %d, want 200", got)
	}
}

func TestFormatPayloadSanitizes(t *testing.T) {
	cases := []struct{ typ, raw string }{
		{"torrent.added", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=%3Cscript%3E%0d%0aFAKE%20LOG"},
		{"media.new_file", "movie|../../etc/passwd\nINJECTED"},
		{"disk.warning", strings.Repeat("A", 10_000)},
		{"music.played", `{"artist":"<script>","title":"</script>"}`},
		{"unknown.event", "\x1b[31mansi\r\ninject"},
	}
	for _, tc := range cases {
		out := formatPayload(tc.typ, tc.raw)
		if !utf8.ValidString(out) {
			t.Errorf("%s: invalid utf8: %q", tc.typ, out)
		}
		if strings.ContainsAny(out, "\r\n\x00\x1b") {
			t.Errorf("%s: control chars в payload: %q", tc.typ, out)
		}
		if n := utf8.RuneCountInString(out); n > maxPayloadRunes {
			t.Errorf("%s: payload не обрезан: %d runes", tc.typ, n)
		}
	}
}

func TestMagnetDisplayName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"magnet:?xt=urn:btih:0123456789abcdef&dn=Ubuntu+ISO", "Ubuntu ISO"},
		{"magnet:?dn=C%2B%2B+Guide", "C++ Guide"},
		{"magnet:?dn=First&dn=Second+Name", "Second Name"},
		{"magnet:?xdn=evil&dn=Real", "Real"},
		{"not-a-magnet", ""},
	}
	for _, tc := range cases {
		if got := magnetDisplayName(tc.in); got != tc.want {
			t.Errorf("magnetDisplayName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMagnetHashPrefixRejectsNonHex(t *testing.T) {
	bad := []string{
		"magnet:?xt=urn:btih:<script>alert(1)</script>",
		"magnet:?xt=urn:btih:zzzzzzzz",
		"magnet:?xt=urn:btih:",
	}
	for _, in := range bad {
		if got := magnetHashPrefix(in); got != "" {
			t.Errorf("magnetHashPrefix(%q) = %q, want empty", in, got)
		}
	}
	const hash = "0123456789abcdef0123456789abcdef01234567"
	if got := magnetHashPrefix("magnet:?xt=urn:btih:" + hash); got != hash[:16] {
		t.Fatalf("valid btih prefix = %q, want %q", got, hash[:16])
	}
}

func TestTruncateUTF8AndControlChars(t *testing.T) {
	if got := truncate("привет мир", 6); got != "привет" {
		t.Fatalf("truncate = %q, want привет", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Fatalf("short string changed: %q", got)
	}
	if !utf8.ValidString(truncate(strings.Repeat("я", 1000), 10)) {
		t.Fatal("truncate broke utf8")
	}
}

func TestSanitizeText(t *testing.T) {
	got := sanitizeText("a\r\nb\x00c\x1b[31m")
	if strings.ContainsAny(got, "\r\n\x00\x1b") {
		t.Fatalf("control chars остались: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("invalid utf8: %q", got)
	}
}

func TestParseTimeParam(t *testing.T) {
	ts, err := parseTimeParam("2026-10-20", true)
	if err != nil {
		t.Fatalf("parse date: %v", err)
	}
	if ts.Format("2006-01-02") != "2026-10-20" || ts.Hour() != 23 || ts.Minute() != 59 {
		t.Fatalf("end-of-day = %v", ts)
	}
	if _, err := parseTimeParam("garbage", false); err == nil {
		t.Fatal("ожидалась ошибка для невалидной даты")
	}
}

func TestClampInt(t *testing.T) {
	cases := []struct {
		raw           string
		def, min, max int
		want          int
	}{
		{"", 50, 1, 500, 50},
		{"0", 50, 1, 500, 50},
		{"-1", 50, 1, 500, 50},
		{"1000", 50, 1, 500, 500},
		{"42", 50, 1, 500, 42},
	}
	for _, tc := range cases {
		if got := clampInt(tc.raw, tc.def, tc.min, tc.max); got != tc.want {
			t.Errorf("clampInt(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestEscapeLike(t *testing.T) {
	got := escapeLike(`100%_done\`)
	want := `100\%\_done\\`
	if got != want {
		t.Fatalf("escapeLike = %q, want %q", got, want)
	}
}

func TestGuessDownloadCategory(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Some.Show.S01E02.1080p", "tv"},
		{"Album FLAC discography", "music"},
		{"Game Repack FitGirl", "games"},
		{"Udemy course", "courses"},
		{"Some Book .epub", "books"},
		{"Movie 2024 2160p", "movies"},
	}
	for _, tc := range cases {
		if got := guessDownloadCategory(tc.in); got != tc.want {
			t.Errorf("guessDownloadCategory(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
