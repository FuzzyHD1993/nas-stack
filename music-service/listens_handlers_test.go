package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestDeleteListensRequiresConfirm(t *testing.T) {
	app := fiber.New()
	listensRoutes(app)

	resp, err := app.Test(httptest.NewRequest("DELETE", "/listens", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (confirm не передан)", resp.StatusCode)
	}
}

func TestSmartPlaylistUnknownType(t *testing.T) {
	app := fiber.New()
	listensRoutes(app)

	resp, err := app.Test(httptest.NewRequest("GET", "/playlists/smart?type=nope", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
