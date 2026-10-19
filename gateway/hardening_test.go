package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
)

func TestAdminTokenFromEnv(t *testing.T) {
	if _, err := adminTokenFromEnv(func(string) string { return "   " }); err == nil {
		t.Fatal("пустой/пробельный ADMIN_TOKEN должен возвращать ошибку")
	}
	got, err := adminTokenFromEnv(func(string) string { return "  secret  " })
	if err != nil || got != "secret" {
		t.Fatalf("got = %q, err = %v", got, err)
	}
}

func TestAdminProtectedPathEncodedBypass(t *testing.T) {
	for _, p := range []string{
		"update/run",
		"//update//run",
		"update/./run",
		"update%2Frun",
		"update%252Frun",
		`update\run`,
		"foo/../update/run",
		"env",
		"/env",
		"env/reload",
		"env%2Freload",
	} {
		if !adminProtectedPath("notify", "POST", p) {
			t.Errorf("adminProtectedPath(%q) = false, ожидалось true", p)
		}
	}
	if adminProtectedPath("notify", "GET", "health") {
		t.Error("health не должен быть admin-путём")
	}
	if adminProtectedPath("notify", "POST", "send") {
		t.Error("send не должен быть admin-путём")
	}
	if !serviceTokenPath("notify", "POST", "send%2F") {
		t.Error("serviceTokenPath не нормализовал путь")
	}
}

func TestValidateProxyPath(t *testing.T) {
	for _, p := range []string{
		"../etc/passwd",
		"%2e%2e/%2e%2e/etc/passwd",
		`..\..\etc\passwd`,
		"ok/../../etc",
		"..%252f..%252fetc/passwd",
		"health%00",
	} {
		if err := validateProxyPath(p); err == nil {
			t.Errorf("validateProxyPath(%q) = nil, ожидалась ошибка", p)
		}
	}
	for _, p := range []string{"books/123/cover.jpg", "http://evil.example/x"} {
		if err := validateProxyPath(p); err != nil {
			t.Fatalf("валидный путь %q отклонён: %v", p, err)
		}
	}
}

func TestProxyHandlerRejectsTraversal(t *testing.T) {
	withSecrets(t, "admin-secret", "notify-secret")

	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	app := fiber.New()
	app.All("/notify/*", proxyHandler(upstream.URL, "notify"))

	for _, target := range []string{
		"/notify/../scheduler/update/run",
		"/notify/%2e%2e/scheduler/update/run",
		"/notify/..%252f..%252fscheduler/update/run",
		"/notify/..%255c..%255cenv",
	} {
		resp := do(t, app, "POST", target, nil)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("%s: запрос прошёл (%d)", target, resp.StatusCode)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("upstream вызван %d раз", n)
	}
}

func TestRequireAuthRejectsNonHS256(t *testing.T) {
	withSecrets(t, "", "")

	const secret = "test-secret"
	app := fiber.New()
	app.Get("/status", requireAuthMiddleware([]byte(secret), true), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	noneTok, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "user",
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	hs256, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user",
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{
		"sub": "user",
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"alg none", noneTok, http.StatusUnauthorized},
		{"HS512", hs512, http.StatusUnauthorized},
		{"valid HS256", hs256, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]string{}
			if tc.token != "" {
				h["Authorization"] = "Bearer " + tc.token
			}
			if resp := do(t, app, "GET", "/status", h); resp.StatusCode != tc.want {
				t.Fatalf("статус %d, ожидался %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestSanitizeImageContentType(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":               "image/jpeg",
		"image/png; charset=utf-8": "image/png",
		"text/html":                "image/jpeg",
		"":                         "image/jpeg",
		"application/octet-stream": "image/jpeg",
	}
	for in, want := range cases {
		if got := sanitizeImageContentType(in); got != want {
			t.Errorf("sanitizeImageContentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSmartQueryRuntimeBounds(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]interface{}
	}{
		{"999999999999999999999 минут", map[string]interface{}{"limit": 50}},
		{"999999 минут", map[string]interface{}{"runtime_max": smartMaxRuntime + 10, "limit": 50}},
		{"0 минут", map[string]interface{}{"limit": 50}},
		{"короткий длинный фильм", map[string]interface{}{"limit": 50}},
		{"длинный фильм 90 минут", map[string]interface{}{"runtime_max": 100, "limit": 50}},
		{"короткий фильм 120 минут", map[string]interface{}{"runtime_max": 130, "limit": 50}},
	}
	for _, tc := range cases {
		if got := parseSmartQuery(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseSmartQuery(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}
