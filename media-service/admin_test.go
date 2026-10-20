package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestRequireMediaAdmin(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret-token-0123456789")

	app := fiber.New()
	app.Post("/x", requireMediaAdmin, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	req := httptest.NewRequest(fiber.MethodPost, "/x", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}

	req = httptest.NewRequest(fiber.MethodPost, "/x", nil)
	req.Header.Set("Authorization", "Bearer secret-token-0123456789")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("valid token = %d, want 204", resp.StatusCode)
	}
}

func TestRequireMediaAdminFailsClosed(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")

	app := fiber.New()
	app.Delete("/x", requireMediaAdmin, func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusNoContent)
	})

	req := httptest.NewRequest(fiber.MethodDelete, "/x", nil)
	req.Header.Set("X-Admin-Token", "anything")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("empty ADMIN_TOKEN = %d, want 401", resp.StatusCode)
	}
}
