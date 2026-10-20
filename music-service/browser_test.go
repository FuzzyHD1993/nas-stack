package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func resetMusicIndex() {
	musicIdxMu.Lock()
	musicIdx = nil
	musicIdxTime = time.Time{}
	musicIdxMu.Unlock()
}

func TestSafeMusicPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("MUSIC_DIR", base)
	if _, ok := safeMusicPath("Artist/Album/track.flac"); !ok {
		t.Error("valid path rejected")
	}
	if _, ok := safeMusicPath("../../etc/passwd"); ok {
		t.Error("traversal allowed")
	}
}

func TestSafeMusicPathSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	t.Setenv("MUSIC_DIR", base)

	if err := os.WriteFile(filepath.Join(outside, "secret.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.flac"), filepath.Join(base, "evil.flac")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, ok := safeMusicFile("evil.flac"); ok {
		t.Fatal("symlink outside MUSIC_DIR accepted")
	}

	if err := os.Mkdir(filepath.Join(base, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "Album", "ok.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := safeMusicFile("Album/ok.flac"); !ok {
		t.Fatal("regular file rejected")
	}
	if _, ok := safeMusicFile("Album"); ok {
		t.Fatal("directory accepted as audio file")
	}
}

func TestBrowserAlbums(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "Deep Purple", "Machine Head")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "01.flac"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "02.flac"), []byte("x"), 0o644)
	t.Setenv("MUSIC_DIR", base)
	resetMusicIndex()

	albums := browserAlbums()
	if len(albums) != 1 {
		t.Fatalf("albums = %v", albums)
	}
	if albums[0]["artist"] != "Deep Purple" || albums[0]["album"] != "Machine Head" {
		t.Errorf("album = %v", albums[0])
	}
	tracks := browserAlbumTracks("Deep Purple/Machine Head")
	if len(tracks) != 2 {
		t.Fatalf("tracks = %v", tracks)
	}
}
