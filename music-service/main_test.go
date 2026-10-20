package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
)

func TestGetEnv(t *testing.T) {
	t.Setenv("__NAS_TEST_MISSING__", "")
	if got := platform.GetEnv("__NAS_TEST_MISSING__", "def"); got != "def" {
		t.Fatalf("getEnv default = %q, want %q", got, "def")
	}
	t.Setenv("__NAS_TEST_MISSING__", "value")
	if got := platform.GetEnv("__NAS_TEST_MISSING__", "def"); got != "value" {
		t.Fatalf("getEnv = %q, want value", got)
	}
}

func TestHealthJSON(t *testing.T) {
	m := platform.HealthJSON("test-service", fiber.Map{"echo": "hi"})
	if m["status"] != "ok" {
		t.Fatalf("status = %v, want ok", m["status"])
	}
	if m["service"] != "test-service" {
		t.Fatalf("service = %v", m["service"])
	}
	for _, k := range []string{"goroutines", "mem_mb", "uptime_sec", "gc_count"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing metric %q", k)
		}
	}
	if m["echo"] != "hi" {
		t.Fatalf("extra not merged: %v", m["echo"])
	}
}

func TestValidateEnv(t *testing.T) {
	t.Setenv("__V101_REQUIRED__", "")
	if err := platform.ValidateEnv([]string{"__V101_REQUIRED__"}); err == nil {
		t.Error("ожидалась ошибка для отсутствующей переменной")
	}
	t.Setenv("__V101_REQUIRED__", "   ")
	if err := platform.ValidateEnv([]string{"__V101_REQUIRED__"}); err == nil {
		t.Error("пробельное значение должно считаться отсутствующим")
	}
	t.Setenv("__V101_REQUIRED__", "x")
	if err := platform.ValidateEnv([]string{"__V101_REQUIRED__"}); err != nil {
		t.Errorf("неожиданная ошибка: %v", err)
	}
	if err := platform.ValidateEnv(nil); err != nil {
		t.Fatalf("пустой список: %v", err)
	}
}

// Альбом с треками без тега Track не должен паниковать.
func TestTrackNoMissingTag(t *testing.T) {
	tr := []fiber.Map{
		{"file": "b.flac"},
		{"file": "a.flac", "track": "1"},
		{"file": "c.flac", "track": "2/12"},
	}
	sort.SliceStable(tr, func(i, j int) bool { return trackNo(tr[i]) < trackNo(tr[j]) })
	if tr[2]["file"] != "c.flac" || tr[1]["file"] != "a.flac" {
		t.Fatalf("unexpected order: %v", tr)
	}

	cases := []struct {
		m    fiber.Map
		want int
	}{
		{fiber.Map{}, 0},
		{fiber.Map{"track": "03"}, 3},
		{fiber.Map{"track": "3/12"}, 3},
		{fiber.Map{"track": float64(7)}, 7},
		{fiber.Map{"track": "abc"}, 0},
		{fiber.Map{"track": "-1"}, 0},
	}
	for _, tc := range cases {
		if got := trackNo(tc.m); got != tc.want {
			t.Errorf("trackNo(%v) = %d, want %d", tc.m, got, tc.want)
		}
	}
}

// MPD-команды не должны допускать инъекцию через перевод строки.
func TestMPDQuoteNoNewlineInjection(t *testing.T) {
	for _, in := range []string{"x\nstop", "x\r\nclear", "a\x00b", "\n", "x\r"} {
		if got := mpdQuote(in); strings.ContainsAny(got, "\r\n\x00") {
			t.Fatalf("mpdQuote(%q) = %q: незаэкранированный перевод строки", in, got)
		}
	}
}

func TestValidPlaylistName(t *testing.T) {
	ok := []string{"Мой плейлист", "mix-2024", "a"}
	for _, s := range ok {
		if !validPlaylistName(s) {
			t.Errorf("validPlaylistName(%q) = false, want true", s)
		}
	}
	bad := []string{"", ".", "..", "../x", "a/b", `a\b`, "a\nb", "x\x00y"}
	for _, s := range bad {
		if validPlaylistName(s) {
			t.Errorf("validPlaylistName(%q) = true, want false", s)
		}
	}
}
