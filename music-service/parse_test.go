package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMPDQuote(t *testing.T) {
	cases := map[string]string{
		"":       `""`,
		"abc":    "abc",
		"a b":    `"a b"`,
		`a"b`:    `"a\"b"`,
		`a\b c`:  `"a\\b c"`,
		"a\nb":   "ab",
		"a\r\nb": "ab",
	}
	for in, want := range cases {
		if got := mpdQuote(in); got != want {
			t.Errorf("mpdQuote(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParseAudioInfo(t *testing.T) {
	old := bitPerfectEnabled.Load()
	bitPerfectEnabled.Store(false)
	t.Cleanup(func() { bitPerfectEnabled.Store(old) })

	ai := parseAudioInfo("track.flac", map[string]string{"audio": "96000:24:2", "bitrate": "4608"})
	if ai.Codec != "flac" {
		t.Errorf("codec = %q", ai.Codec)
	}
	if ai.SampleRate != 96000 || ai.Bits != 24 || ai.Channels != 2 {
		t.Errorf("audio = %d/%d/%d", ai.SampleRate, ai.Bits, ai.Channels)
	}
	if ai.BitrateKbps != 4608 {
		t.Errorf("bitrate = %d, want 4608", ai.BitrateKbps)
	}
	if ai.Label != "FLAC 24bit/96.0kHz" {
		t.Errorf("label = %q", ai.Label)
	}
}

func TestParseAudioInfoMalformed(t *testing.T) {
	old := bitPerfectEnabled.Load()
	bitPerfectEnabled.Store(false)
	t.Cleanup(func() { bitPerfectEnabled.Store(old) })

	cases := []struct {
		audio   string
		bitrate string
		file    string
	}{
		{"", "", ""},
		{"", "", "t.flac"},
		{"96000:24", "", "t.flac"},
		{"abc:def:ghi", "", "t.flac"},
		{"999999999:999999:999999", "", "t.flac"},
		{"0:0:0", "", "t.flac"},
		{"44100:16:2", "", "track.FLAC"},
		{"2822400:1:2", "", "t.dsf"},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %+v: %v", c, r)
				}
			}()
			info := parseAudioInfo(c.file, map[string]string{"audio": c.audio, "bitrate": c.bitrate})
			if info.BitrateKbps < 0 {
				t.Fatalf("negative bitrate for %+v", c)
			}
		}()
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
		"file: a/one.flac", "Title: One", "Artist: A", "Album: Al", "duration: 123.5",
		"file: a/two.flac", "Title: Two", "Artist: A", "Album: Al", "duration: 200",
	}
	tracks := parseTrackList(lines)
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(tracks))
	}
	if tracks[0]["title"] != "One" || tracks[1]["artist"] != "A" {
		t.Errorf("unexpected parse: %v", tracks)
	}
	if d, ok := tracks[0]["duration"].(float64); !ok || d != 123.5 {
		t.Fatalf("duration = %T %v, want float64 123.5", tracks[0]["duration"], tracks[0]["duration"])
	}
	if _, exists := tracks[0]["time"]; exists {
		t.Fatal("ключ time не должен попадать в трек")
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
		"Album 1999 Deluxe Edition":          "Album",
		"Live at Leeds":                      "Live at Leeds",
		"Live After Death":                   "Live After Death",
		"  Live  ":                           "Live",
		"Made In Europe":                     "Made In Europe",
	}
	for in, want := range cases {
		if got := normalizeForSearch(in); got != want {
			t.Errorf("normalizeForSearch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNextAlbumTracksRejectsEscape(t *testing.T) {
	base := t.TempDir()
	t.Setenv("MUSIC_DIR", base)

	for _, in := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"Artist/../../../etc/x.flac",
		"..",
		"./../../x.flac",
		"Artist/Album\x00/x.flac",
	} {
		tracks, next := nextAlbumTracks(in)
		if len(tracks) != 0 || next != "" {
			t.Errorf("%q: traversal не отклонён: %v / %q", in, tracks, next)
		}
	}
}
