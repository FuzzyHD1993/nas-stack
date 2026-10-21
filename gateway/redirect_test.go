package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestSanitizeRedirect(t *testing.T) {
	cases := []struct {
		name string
		loc  string
		want string
	}{
		{"magnet", "magnet:?xt=urn:btih:abc&dn=x", "magnet:?xt=urn:btih:abc&dn=x"},
		{"same origin", "http://gateway.local/next", "http://gateway.local/next"},
		{"relative", "/next", "/next"},
		{"foreign origin", "http://evil.example/x", ""},
		{"scheme relative", "//evil.example/x", ""},
		{"javascript", "javascript:alert(1)", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()
			var got string
			app.Get("/", func(c fiber.Ctx) error {
				got = sanitizeRedirect(c, tc.loc)
				return c.SendString("ok")
			})
			req := httptest.NewRequest(http.MethodGet, "http://gateway.local/", nil)
			if _, err := app.Test(req); err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			if got != tc.want {
				t.Fatalf("sanitizeRedirect(%q) = %q, want %q", tc.loc, got, tc.want)
			}
		})
	}
}
