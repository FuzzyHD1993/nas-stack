package main

import (
	"testing"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

func TestGetEnv(t *testing.T) {
	if got := platform.GetEnv("__NAS_TEST_MISSING__", "def"); got != "def" {
		t.Fatalf("getEnv default = %q, want %q", got, "def")
	}
}

func TestIsSecret(t *testing.T) {
	cases := map[string]bool{
		"JWT_SECRET":       true,
		"RUTRACKER_PASS":   true,
		"NTFY_TOKEN":       true,
		"PROWLARR_API_KEY": true,
		"FLIBUSTA_URL":     false,
		"MEDIA_MOVIES_DIR": false,
	}
	for k, want := range cases {
		if got := isSecret(k); got != want {
			t.Fatalf("isSecret(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestTruncateRuneSafe(t *testing.T) {
	s := "Привет мир"
	got := truncate(s, 3)
	if got != "При..." {
		t.Fatalf("truncate = %q, want %q", got, "При...")
	}
}
