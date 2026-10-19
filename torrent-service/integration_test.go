package main

import "testing"

func TestParseRuSize(t *testing.T) {
	cases := []struct {
		val, unit string
		want      float64
	}{
		{"1,5", "GB", 1.5}, {"2", "TB", 2048}, {"512", "MB", 0.5}, {"7", "??", 7},
	}
	for _, c := range cases {
		if got := parseRuSize(c.val, c.unit); got != c.want {
			t.Errorf("parseRuSize(%q,%q)=%v want %v", c.val, c.unit, got, c.want)
		}
	}
}

func TestMagnetName(t *testing.T) {
	m := "magnet:?xt=urn:btih:abc&dn=Hello+World&tr=udp://x"
	if got := magnetName(m); got != "Hello World" {
		t.Errorf("magnetName = %q", got)
	}
}

func TestGuessCategory(t *testing.T) {
	cases := map[string]string{
		"Some.Show.S01E01.1080p": "tv",
		"Album FLAC discography": "music",
		"FitGirl repack":         "games",
		"Udemy course python":    "courses",
		"Some Book epub":         "books",
		"Movie 2024 1080p":       "movies",
	}
	for in, want := range cases {
		if got := guessCategory(in); got != want {
			t.Errorf("guessCategory(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRuToUTF8(t *testing.T) {
	if got := ruToUTF8("Привет"); got != "Привет" {
		t.Errorf("valid utf8 changed: %q", got)
	}
	// CP1251 "Привет" = 0xCF 0xF0 0xE8 0xE2 0xE5 0xF2
	cp := string([]byte{0xCF, 0xF0, 0xE8, 0xE2, 0xE5, 0xF2})
	if got := ruToUTF8(cp); got != "Привет" {
		t.Errorf("cp1251 decode = %q", got)
	}
}
