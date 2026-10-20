package main

import (
	"math"
	"testing"
)

func TestParseFloat(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want float64
	}{
		{"decimal", "12.5", 12.5},
		{"zero", "0", 0},
		{"spaces", " 7 ", 7},
		{"garbage", "bad", 0},
		{"empty", "", 0},
		{"nan", "NaN", 0},
		{"inf", "Inf", 0},
		{"neg_inf", "-Inf", 0},
		{"overflow", "1e400", 0},
		{"negative", "-1", 0},
		{"too_long", "99999999", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseFloat(tc.in); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("parseFloat(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPeriodDays(t *testing.T) {
	cases := map[string]int{"week": 7, "month": 30, "year": 365, "": 30, "unknown": 30}
	for in, want := range cases {
		if got := periodDays(in); got != want {
			t.Errorf("periodDays(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestTrackMap(t *testing.T) {
	m := trackMap("Artist/Album/01 - Song.flac", "A", "B", "")
	if m["track_path"] != "Artist/Album/01 - Song.flac" {
		t.Errorf("track_path = %v", m["track_path"])
	}
	if m["title"] != "01 - Song" {
		t.Errorf("title fallback = %v, want %q", m["title"], "01 - Song")
	}
	m2 := trackMap("x.flac", "A", "B", "Real Title")
	if m2["title"] != "Real Title" {
		t.Errorf("title = %v, want Real Title", m2["title"])
	}
}

func TestIsIgnoredTrack(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Artist/Testament/01.flac", false},
		{"Artist/Greatest Hits/01.flac", false},
		{"Artist/Contest/01.flac", false},
		{"Artist/Sampler/01.flac", false},
		{"Artist/Samples/01.flac", true},
		{"test_foo/01.flac", true},
		{"Artist/Album/sample_01.flac", true},
		{"test.flac", true},
		{"preview.mp3", true},
		{"Artist/Album/01.flac", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isIgnoredTrack(tc.in); got != tc.want {
				t.Fatalf("isIgnoredTrack(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
