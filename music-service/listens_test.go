package main

import "testing"

func TestParseFloat(t *testing.T) {
	cases := map[string]float64{
		"12.5": 12.5,
		"0":    0,
		" 7 ":  7,
		"bad":  0,
		"":     0,
	}
	for in, want := range cases {
		if got := parseFloat(in); got != want {
			t.Errorf("parseFloat(%q) = %v, want %v", in, got, want)
		}
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
