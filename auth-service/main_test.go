package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
)

func testCfg(authEnabled bool) config {
	return config{
		password:    "s3cret-password",
		jwtSecret:   []byte("0123456789abcdef0123456789abcdef"),
		authEnabled: authEnabled,
		tokenTTL:    time.Hour,
	}
}

func doRequest(t *testing.T, app *fiber.App, method, path, body, token string) (*http.Response, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	_ = resp.Body.Close()
	return resp, decoded
}

func TestLogin(t *testing.T) {
	cfg := testCfg(true)
	app := newApp(cfg, nil)

	t.Run("wrong password", func(t *testing.T) {
		resp, body := doRequest(t, app, http.MethodPost, "/login", `{"password":"wrong"}`, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if _, ok := body["token"]; ok {
			t.Fatal("токен не должен выдаваться при неверном пароле")
		}
	})

	t.Run("ok", func(t *testing.T) {
		resp, body := doRequest(t, app, http.MethodPost, "/login", `{"password":"s3cret-password"}`, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("ожидался Cache-Control: no-store")
		}
		token, _ := body["token"].(string)
		claims, err := parseToken(token, cfg.jwtSecret)
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		if claims["scope"] != scopeAdmin {
			t.Fatalf("scope = %v, want %s", claims["scope"], scopeAdmin)
		}
	})
}

func TestLoginDisabledWhenAuthOff(t *testing.T) {
	app := newApp(testCfg(false), nil)
	resp, _ := doRequest(t, app, http.MethodPost, "/login", `{"password":"s3cret-password"}`, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestLoginRateLimited(t *testing.T) {
	app := newApp(testCfg(true), nil)
	var status int
	for i := 0; i < 11; i++ {
		resp, _ := doRequest(t, app, http.MethodPost, "/login", `{"password":"wrong"}`, "")
		status = resp.StatusCode
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
}

func TestVerifyRejectsWeakTokens(t *testing.T) {
	cfg := testCfg(true)
	app := newApp(cfg, nil)

	valid, _ := issueToken(cfg.jwtSecret, scopeAdmin, 0, time.Hour)
	profileScope, _ := issueToken(cfg.jwtSecret, "profile", 1, time.Hour)
	expired, _ := issueToken(cfg.jwtSecret, scopeAdmin, 0, -time.Hour)

	foreign, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user", "iss": tokenIssuer, "scope": scopeAdmin,
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("another-secret-another-secret-32b"))

	noExp, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user", "iss": tokenIssuer, "scope": scopeAdmin,
	}).SignedString(cfg.jwtSecret)

	badIssuer, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user", "iss": "evil", "scope": scopeAdmin,
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(cfg.jwtSecret)

	hs512, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{
		"sub": "user", "iss": tokenIssuer, "scope": scopeAdmin,
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(cfg.jwtSecret)

	noneTok, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "user", "iss": tokenIssuer, "scope": scopeAdmin,
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"garbage", "not.a.jwt", http.StatusUnauthorized},
		{"alg none", noneTok, http.StatusUnauthorized},
		{"hs512", hs512, http.StatusUnauthorized},
		{"no exp", noExp, http.StatusUnauthorized},
		{"bad issuer", badIssuer, http.StatusUnauthorized},
		{"foreign secret", foreign, http.StatusUnauthorized},
		{"expired", expired, http.StatusUnauthorized},
		{"profile scope", profileScope, http.StatusUnauthorized},
		{"valid", valid, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := doRequest(t, app, http.MethodGet, "/verify", "", tc.token)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestProfilesRequireAdminScope(t *testing.T) {
	cfg := testCfg(true)
	app := newApp(cfg, nil)

	resp, _ := doRequest(t, app, http.MethodGet, "/profiles", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /profiles без токена: status = %d, want 401", resp.StatusCode)
	}

	profileToken, _ := issueToken(cfg.jwtSecret, "profile", 1, time.Hour)
	resp, _ = doRequest(t, app, http.MethodGet, "/profiles", "", profileToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /profiles с profile-scope: status = %d, want 403", resp.StatusCode)
	}

	adminToken, _ := issueToken(cfg.jwtSecret, scopeAdmin, 0, time.Hour)
	resp, _ = doRequest(t, app, http.MethodGet, "/profiles", "", adminToken)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /profiles с токеном и nil DB: status = %d, want 503", resp.StatusCode)
	}

	resp, _ = doRequest(t, app, http.MethodPost, "/profiles/1/select", `{"pin":""}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("select без токена: status = %d, want 401", resp.StatusCode)
	}
}

func TestProfilesOpenWhenAuthDisabled(t *testing.T) {
	app := newApp(testCfg(false), nil)
	resp, _ := doRequest(t, app, http.MethodGet, "/profiles", "", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (auth выключен, БД нет)", resp.StatusCode)
	}
}

func TestSelectRejectsMalformedID(t *testing.T) {
	cfg := testCfg(true)
	app := newApp(cfg, nil)
	token, _ := issueToken(cfg.jwtSecret, scopeAdmin, 0, time.Hour)

	for _, id := range []string{"abc", "0", "-1"} {
		resp, _ := doRequest(t, app, http.MethodPost, "/profiles/"+id+"/select", `{}`, token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("id=%q: status = %d, want 400", id, resp.StatusCode)
		}
	}
}

func TestPINHashing(t *testing.T) {
	h, err := hashPIN("1234")
	if err != nil {
		t.Fatal(err)
	}
	if ok, rehash := verifyPIN(h, "1234"); !ok || rehash {
		t.Fatalf("bcrypt PIN не прошёл: ok=%v rehash=%v", ok, rehash)
	}
	if ok, _ := verifyPIN(h, "12345"); ok {
		t.Fatal("неверный PIN прошёл проверку")
	}

	legacy := legacyHashPIN("1234")
	if ok, rehash := verifyPIN(legacy, "1234"); !ok || !rehash {
		t.Fatalf("legacy PIN: ok=%v rehash=%v", ok, rehash)
	}
	if ok, _ := verifyPIN(legacy, ""); ok {
		t.Fatal("пустой PIN не должен проходить для legacy-хэша")
	}
	if ok, _ := verifyPIN("", ""); !ok {
		t.Fatal("пустой pin_hash должен означать «PIN не задан»")
	}
}
