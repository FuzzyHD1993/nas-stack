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
