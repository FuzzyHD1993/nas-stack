package main

import "testing"

func TestToHostPath(t *testing.T) {
	cases := []struct {
		in, typ, want string
	}{
		{"/movies/Foo (2020)/foo.mkv", "movie", "/mnt/nas/root/FuzzyHD/Movies/Foo (2020)/foo.mkv"},
		{"/tv/Show/S01E01.mkv", "tv", "/mnt/nas/root/FuzzyHD/TV/Show/S01E01.mkv"},
	}
	for _, tc := range cases {
		if got := toHostPath(tc.in, tc.typ); got != tc.want {
			t.Errorf("toHostPath(%q,%q) = %q, want %q", tc.in, tc.typ, got, tc.want)
		}
	}
}

func TestParseName(t *testing.T) {
	cases := []struct {
		in, title string
		year      int
	}{
		{"The.Matrix.1999.1080p.BluRay.mkv", "The Matrix", 1999},
		{"Inception.2010.2160p.UHD.mkv", "Inception", 2010},
		{"Матрица (1999) 1080p.mkv", "Матрица", 1999},
	}
	for _, c := range cases {
		gotTitle, gotYear := parseName(c.in)
		if gotTitle != c.title {
			t.Errorf("parseName(%q) title = %q, want %q", c.in, gotTitle, c.title)
		}
		if gotYear != c.year {
			t.Errorf("parseName(%q) year = %d, want %d", c.in, gotYear, c.year)
		}
	}
}

func TestStripExtensionOnlyVideo(t *testing.T) {
	if got := stripExtension("movie.mkv"); got != "movie" {
		t.Errorf("mkv: %q", got)
	}
	if got := stripExtension("song.flac"); got != "song.flac" {
		t.Errorf("flac must be untouched, got %q", got)
	}
}

func TestIsVideoAudioExt(t *testing.T) {
	if !isVideoExt(".mkv") || !isVideoExt(".mp4") {
		t.Error("video ext not detected")
	}
	if !isAudioExt(".flac") || !isAudioExt(".mp3") {
		t.Error("audio ext not detected")
	}
	if isVideoExt(".flac") {
		t.Error("flac must not be video")
	}
}
