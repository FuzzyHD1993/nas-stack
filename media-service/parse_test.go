package main

import "testing"

func TestToHostPath(t *testing.T) {
	t.Setenv("MEDIA_MOVIES_DIR", "/movies")
	t.Setenv("MEDIA_TV_DIR", "/tv")
	t.Setenv("MEDIA_MOVIES_HOST_DIR", "/mnt/nas/root/FuzzyHD/Movies")
	t.Setenv("MEDIA_TV_HOST_DIR", "/mnt/nas/root/FuzzyHD/TV")

	cases := []struct {
		in, typ, want string
	}{
		{"/movies/Foo (2020)/foo.mkv", "movie", "/mnt/nas/root/FuzzyHD/Movies/Foo (2020)/foo.mkv"},
		{"/tv/Show/S01E01.mkv", "tv", "/mnt/nas/root/FuzzyHD/TV/Show/S01E01.mkv"},
	}
	for _, tc := range cases {
		got, err := toHostPath(tc.in, tc.typ)
		if err != nil {
			t.Fatalf("toHostPath(%q,%q): %v", tc.in, tc.typ, err)
		}
		if got != tc.want {
			t.Errorf("toHostPath(%q,%q) = %q, want %q", tc.in, tc.typ, got, tc.want)
		}
	}
}

func TestToHostPathRejectsEscapes(t *testing.T) {
	t.Setenv("MEDIA_MOVIES_DIR", "/movies")
	t.Setenv("MEDIA_TV_DIR", "/tv")
	t.Setenv("MEDIA_MOVIES_HOST_DIR", "/mnt/nas/root/FuzzyHD/Movies")
	t.Setenv("MEDIA_TV_HOST_DIR", "/mnt/nas/root/FuzzyHD/TV")

	for _, tc := range []struct {
		in, typ string
	}{
		{"/movies/../etc/passwd", "movie"},
		{"/etc/passwd", "movie"},
		{"/movies", "movie"},
		{"/tv", "tv"},
		{"/movies-evil/x.mkv", "movie"},
		{"/tv/Show/S01E01.mkv", "movie"},
		{"/movies/../TV/x.mkv", "movie"},
		{"/movies/x.mkv", ""},
		{"/movies/x.mkv", "photo"},
		{"movies/x.mkv", "movie"},
	} {
		if got, err := toHostPath(tc.in, tc.typ); err == nil {
			t.Errorf("toHostPath(%q,%q) = %q, want error", tc.in, tc.typ, got)
		}
	}
}

func TestPosterURLAllowed(t *testing.T) {
	for _, raw := range []string{
		"https://image.tmdb.org/t/p/w500/a.jpg",
		"https://image.tmdb.org/t/p/original/a.jpg?x=1",
	} {
		if _, ok := posterURLAllowed(raw); !ok {
			t.Errorf("posterURLAllowed(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"",
		"http://image.tmdb.org/t/p/w500/a.jpg",
		"https://image.tmdb.org.evil/t/p/w500/a.jpg",
		"https://user@image.tmdb.org/t/p/w500/a.jpg",
		"https://image.tmdb.org/3/movie/1?api_key=x",
		"https://evil.example/t/p/w500/a.jpg",
	} {
		if _, ok := posterURLAllowed(raw); ok {
			t.Errorf("posterURLAllowed(%q) = true, want false", raw)
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
	for _, ext := range []string{".mkv", ".mp4", ".avi", ".m4v", ".mov", ".webm", ".wmv", ".MKV", ".WebM"} {
		if !isVideoExt(ext) {
			t.Errorf("isVideoExt(%q) = false, want true", ext)
		}
		if got := stripExtension("x" + ext); got != "x" {
			t.Errorf("stripExtension(x%s) = %q, want x", ext, got)
		}
	}
	for _, ext := range []string{".flac", ".mp3", ".m4a", ".opus", ".FLAC"} {
		if !isAudioExt(ext) {
			t.Errorf("isAudioExt(%q) = false, want true", ext)
		}
		if isVideoExt(ext) {
			t.Errorf("isVideoExt(%q) = true, audio must not be video", ext)
		}
	}
}

func TestParseNameFallbackStripsExtension(t *testing.T) {
	for _, tc := range []struct {
		in, title string
		year      int
	}{
		{"1917.mkv", "1917", 1917},
		{"1080p.mkv", "1080p", 0},
		{"01 Жуки.mkv", "Жуки", 0},
		{"The Matrix", "The Matrix", 0},
		{"", "", 0},
	} {
		gotTitle, gotYear := parseName(tc.in)
		if gotTitle != tc.title || gotYear != tc.year {
			t.Errorf("parseName(%q) = (%q, %d), want (%q, %d)", tc.in, gotTitle, gotYear, tc.title, tc.year)
		}
	}
}
