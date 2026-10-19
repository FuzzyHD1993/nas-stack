package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseSmartQuery(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]interface{}
	}{
		{"genre_comedy_ru", "комедия", map[string]interface{}{"genre": "комеди"}},
		{"genre_comedy_en", "comedy movie", map[string]interface{}{"genre": "комеди"}},
		{"genre_action", "боевик", map[string]interface{}{"genre": "боевик"}},
		{"runtime_minutes", "фильм 90 минут", map[string]interface{}{"runtime_max": 100}},
		{"runtime_short", "что-то короткое", map[string]interface{}{"runtime_max": 90}},
		{"runtime_long", "длинный фильм", map[string]interface{}{"runtime_min": 150}},
		{"rating_good", "хороший фильм", map[string]interface{}{"rating_min": 7.0}},
		{"rating_masterpiece", "шедевр", map[string]interface{}{"rating_min": 8.0}},
		{"mood_light", "что-то лёгкое на вечер", map[string]interface{}{"genre": "комеди", "rating_min": 6.0}},
		{"year", "фильм 2010 года", map[string]interface{}{"year_from": 2010, "year_to": 2010}},
		{"combo", "лёгкая комедия 90 минут", map[string]interface{}{"genre": "комеди", "runtime_max": 100, "rating_min": 6.0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSmartQuery(tc.in)
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("parseSmartQuery(%q)[%s] = %v, want %v", tc.in, k, got[k], v)
				}
			}
		})
	}
}

func TestParseSmartQueryLimit(t *testing.T) {
	got := parseSmartQuery("asdfghjkl")
	if got["limit"] != 50 {
		t.Errorf("limit = %v, want 50", got["limit"])
	}
	if _, ok := got["genre"]; ok {
		t.Errorf("unexpected genre for gibberish: %v", got["genre"])
	}
}

func TestEnvExampleHasRequired(t *testing.T) {
	data, err := os.ReadFile("../.env.example")
	if err != nil {
		t.Skipf(".env.example недоступен: %v", err)
	}
	content := string(data)
	for _, k := range []string{
		"NAS_ROOT", "LIBRARY_ROOT", "TMDB_API_KEY", "PROWLARR_API_KEY",
		"POSTGRES_URL", "REDIS_ADDR", "JWT_SECRET", "NAS_HOST",
		"MEDIA_MOVIES_HOST_DIR", "MUSIC_HOST_DIR", "TZ",
	} {
		if !strings.Contains(content, k+"=") {
			t.Errorf(".env.example не содержит %s=", k)
		}
	}
}
