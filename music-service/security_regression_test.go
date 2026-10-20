package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithinRootFilepathRel(t *testing.T) {
	if !withinRoot("/", "/etc/passwd") {
		t.Error(`withinRoot("/", "/etc/passwd") = false, want true`)
	}
	if withinRoot("/srv/Music", "/srv/MusicEvil/x") {
		t.Error("sibling prefix accepted")
	}
	if !withinRoot("/srv/Music", "/srv/Music/Album/x.flac") {
		t.Error("nested path rejected")
	}
	if !withinRoot("/srv/Music", "/srv/Music") {
		t.Error("root itself rejected")
	}
}

func TestSafeMusicPathSiblingPrefix(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "Music")
	sibling := filepath.Join(parent, "MusicEvil")
	for _, d := range []string{base, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sibling, "secret.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSIC_DIR", base)

	if _, ok := safeMusicPath("../MusicEvil/secret.flac"); ok {
		t.Fatal("sibling-prefix escape allowed")
	}
	if _, ok := safeMusicFile("../MusicEvil/secret.flac"); ok {
		t.Fatal("sibling file allowed")
	}
}

func TestFindCoverInDirSkipsDirAndBackCover(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cover.jpg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cover-back.jpg", "Artist - Album.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findCoverInDir(dir)
	if err != nil {
		t.Fatalf("findCoverInDir: %v", err)
	}
	if want := filepath.Join(dir, "Artist - Album.jpg"); got != want {
		t.Fatalf("cover = %q, want %q", got, want)
	}
}

func TestFindCoverInDirPrefixPriority(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"artwork-600.jpg", "front-600.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := findCoverInDir(dir)
	if err != nil {
		t.Fatalf("findCoverInDir: %v", err)
	}
	if want := filepath.Join(dir, "front-600.jpg"); got != want {
		t.Fatalf("cover = %q, want %q", got, want)
	}
}

func TestImageContentTypeByMagic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cover.jpg")
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}
	if err := os.WriteFile(p, png, 0o644); err != nil {
		t.Fatal(err)
	}
	if ct := imageContentType(p); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
	if valid := validCoverFile(p); !valid {
		t.Fatal("valid png rejected by validCoverFile")
	}
}

func TestSafeArtworkURL(t *testing.T) {
	ok := []string{
		"https://is1-ssl.mzstatic.com/image/thumb/x/100x100bb.jpg",
		"https://coverartarchive.org/release/x/front-500",
	}
	for _, in := range ok {
		if safeArtworkURL(in) != in {
			t.Errorf("safeArtworkURL(%q) rejected valid URL", in)
		}
	}
	bad := []string{
		"http://is1-ssl.mzstatic.com/x.jpg",
		"https://evil.example/x.jpg",
		"javascript:alert(1)",
		"https://user@is1-ssl.mzstatic.com/x.jpg",
		"//evil.example/x.jpg",
		"",
	}
	for _, in := range bad {
		if got := safeArtworkURL(in); got != "" {
			t.Errorf("safeArtworkURL(%q) = %q, want empty", in, got)
		}
	}
	if got := itunesArtworkURL("https://is1-ssl.mzstatic.com/x/100x100bb.jpg"); !strings.Contains(got, "600x600bb") {
		t.Errorf("itunesArtworkURL = %q, want 600x600bb", got)
	}
}

func TestEscapeLucene(t *testing.T) {
	if got := escapeLucene(`x" OR release:"y`); got != `x\" OR release\:\"y` {
		t.Fatalf("escapeLucene = %q", got)
	}
	if got := coverArtArchiveURL(`../../etc/passwd`); got != "" {
		t.Fatalf("non-UUID accepted: %q", got)
	}
}

func TestCleanListenPath(t *testing.T) {
	if _, ok := cleanListenPath("../../etc/passwd"); ok {
		t.Fatal("traversal allowed")
	}
	if _, ok := cleanListenPath("a/../../etc/passwd"); ok {
		t.Fatal("nested traversal allowed")
	}
	if _, ok := cleanListenPath("http://evil/x.flac"); ok {
		t.Fatal("scheme allowed")
	}
	if _, ok := cleanListenPath("Artist/Album/bad\nname.flac"); ok {
		t.Fatal("control chars allowed")
	}
	if rel, ok := cleanListenPath("Artist/Album/01.flac"); !ok || rel != "Artist/Album/01.flac" {
		t.Fatalf("valid path = %q, %v", rel, ok)
	}
}
