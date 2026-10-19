package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMPDQuote(t *testing.T) {
	cases := map[string]string{
		"":      `""`,
		"abc":   "abc",
		"a b":   `"a b"`,
		`a"b`:   `"a\"b"`,
		`a\b c`: `"a\\b c"`,
	}
	for in, want := range cases {
		if got := mpdQuote(in); got != want {
			t.Errorf("mpdQuote(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseAudioInfo(t *testing.T) {
	old := bitPerfectEnabled
	bitPerfectEnabled = false
	defer func() { bitPerfectEnabled = old }()

	ai := parseAudioInfo("track.flac", map[string]string{"audio": "96000:24:2", "bitrate": "4608"})
	if ai.Codec != "flac" {
		t.Errorf("codec = %q", ai.Codec)
	}
	if ai.SampleRate != 96000 || ai.Bits != 24 || ai.Channels != 2 {
		t.Errorf("audio = %d/%d/%d", ai.SampleRate, ai.Bits, ai.Channels)
	}
	if ai.Label != "FLAC 24bit/96.0kHz" {
		t.Errorf("label = %q", ai.Label)
	}
}

func TestFindCoverInDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Cover.jpg")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findCoverInDir(dir)
	if err != nil || got != p {
		t.Errorf("findCoverInDir = %q, %v; want %q", got, err, p)
	}
}

func TestFindCoverInSubdir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "Scans")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "cover-max.jpg")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findCoverInDir(dir)
	if err != nil || got != p {
		t.Errorf("recursive cover = %q, %v; want %q", got, err, p)
	}
}

func TestParseTrackList(t *testing.T) {
	lines := []string{
		"file: a/one.flac", "Title: One", "Artist: A", "Album: Al", "Time: 123",
		"file: a/two.flac", "Title: Two", "Artist: A", "Album: Al", "Time: 200",
	}
	tracks := parseTrackList(lines)
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(tracks))
	}
	if tracks[0]["title"] != "One" || tracks[1]["artist"] != "A" {
		t.Errorf("unexpected parse: %v", tracks)
	}
}

func TestNextAlbumTracks(t *testing.T) {
	base := t.TempDir()
	a := filepath.Join(base, "Artist", "Album A")
	b := filepath.Join(base, "Artist", "Album B")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "01.flac"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(b, "01.flac"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(b, "02.flac"), []byte("x"), 0o644)
	t.Setenv("MUSIC_DIR", base)

	tracks, next := nextAlbumTracks("Artist/Album A/01.flac")
	if next != "Album B" {
		t.Fatalf("next = %q, want Album B", next)
	}
	if len(tracks) != 2 || tracks[0] != "Artist/Album B/01.flac" {
		t.Fatalf("tracks = %v", tracks)
	}
	if _, n := nextAlbumTracks("Artist/Album B/02.flac"); n != "" {
		t.Fatalf("expected no next album, got %q", n)
	}
}

func TestNormalizeForSearch(t *testing.T) {
	cases := map[string]string{
		"Machine Head (1986, CDP 7 46211 2)": "Machine Head",
		"Machine Head [Remastered]":          "Machine Head",
		"Album 1999 Deluxe Edition":          "Album Edition",
		"  Live  ":                           "",
		"Made In Europe":                     "Made In Europe",
	}
	for in, want := range cases {
		if got := normalizeForSearch(in); got != want {
			t.Errorf("normalizeForSearch(%q) = %q, want %q", in, got, want)
		}
	}
}
