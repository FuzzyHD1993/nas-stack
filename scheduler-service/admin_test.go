package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// P0 host-exec: POST /update/run запускает update-stack.sh через host-agent,
// поэтому должен требовать ADMIN_TOKEN (X-Admin-Token или Bearer).
func TestUpdateRunRequiresAdminToken(t *testing.T) {
	app := fiber.New()
	app.Post("/update/run", requireAdminToken, func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "started"})
	})

	t.Setenv("ADMIN_TOKEN", "topsecret")

	do := func(mode string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/update/run", nil)
		switch mode {
		case "bearer":
			req.Header.Set("Authorization", "Bearer topsecret")
		case "x-admin":
			req.Header.Set("X-Admin-Token", "topsecret")
		case "wrong":
			req.Header.Set("X-Admin-Token", "attacker")
		}
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
		if err != nil {
			return -1, err.Error()
		}
		return resp.StatusCode, ""
	}

	if code, _ := do(""); code != 403 {
		t.Errorf("без токена = %d, ожидался 403", code)
	}
	if code, _ := do("wrong"); code != 403 {
		t.Errorf("с неверным токеном = %d, ожидался 403", code)
	}
	if code, _ := do("bearer"); code != 200 {
		t.Errorf("Bearer ADMIN_TOKEN = %d, ожидался 200", code)
	}
	if code, _ := do("x-admin"); code != 200 {
		t.Errorf("X-Admin-Token = %d, ожидался 200", code)
	}
}

// Fail-closed: без ADMIN_TOKEN никто не запускает обновление стека.
func TestUpdateRunFailsClosedWithoutToken(t *testing.T) {
	app := fiber.New()
	app.Post("/update/run", requireAdminToken, func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "started"})
	})

	t.Setenv("ADMIN_TOKEN", "")
	req := httptest.NewRequest(http.MethodPost, "/update/run", nil)
	req.Header.Set("X-Admin-Token", "anything")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 403 {
		t.Errorf("без секрета даже 'anything' должен получать 403, получено %d", resp.StatusCode)
	}
}
