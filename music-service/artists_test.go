package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestLookupArtistDeterministic(t *testing.T) {
	byName := map[string][]artistTrack{
		"ABBA": {{File: "a1"}},
		"abba": {{File: "a2"}},
	}

	tracks, canonical, ok := lookupArtist(byName, "Abba")
	if !ok {
		t.Fatal("artist not found")
	}
	if canonical != "ABBA" || len(tracks) != 1 || tracks[0].File != "a1" {
		t.Fatalf("canonical=%q tracks=%v", canonical, tracks)
	}
	if _, _, ok := lookupArtist(byName, "missing"); ok {
		t.Fatal("missing artist must not resolve")
	}
}

func TestArtistNameParamKeepsPlus(t *testing.T) {
	app := fiber.New()
	var got string

	app.Get("/x/:name", func(c fiber.Ctx) error {
		v, err := artistNameParam(c)
		if err != nil {
			return err
		}
		got = v
		return c.SendStatus(fiber.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/x/C%2B%2B", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK || got != "C++" {
		t.Fatalf("status=%d got=%q", resp.StatusCode, got)
	}
}

func TestGroupAlbumsCountsUntagged(t *testing.T) {
	albums := groupAlbums([]artistTrack{
		{File: "a", Album: ""},
		{File: "b", Album: "X"},
	})
	if len(albums) != 2 {
		t.Fatalf("albums = %v, want 2 groups", albums)
	}
	if len(albums[noAlbumTitle]) != 1 {
		t.Fatalf("untagged album group: %v", albums[noAlbumTitle])
	}
}
