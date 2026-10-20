package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelMusicPathRejectsEscape(t *testing.T) {
	base := t.TempDir()
	t.Setenv("MUSIC_DIR", base)
	for _, p := range []string{"", "..", "../x", "../../etc/passwd", "/etc/passwd"} {
		if rel, ok := relMusicPath(p); ok {
			t.Errorf("relMusicPath(%q) = %q, want reject", p, rel)
		}
	}
	if rel, ok := relMusicPath("Artist/Album/01.flac"); !ok || rel != "Artist/Album/01.flac" {
		t.Errorf("relMusicPath(valid) = %q, %v", rel, ok)
	}
}

func TestAlbumAudioFilesSkipsNonAudioAndSymlink(t *testing.T) {
	base := t.TempDir()
	t.Setenv("MUSIC_DIR", base)
	album := filepath.Join(base, "Artist", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "evil.flac")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"01.flac", "cover.jpg", ".hidden.flac", "bad\nname.flac"} {
		if err := os.WriteFile(filepath.Join(album, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outsideFile, filepath.Join(album, "link.flac")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	tracks, err := albumAudioFiles("Artist/Album")
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || tracks[0] != "Artist/Album/01.flac" {
		t.Fatalf("tracks = %v, want only 01.flac", tracks)
	}
}

func TestNextAlbumSkipsServiceDirsAndEmpty(t *testing.T) {
	base := t.TempDir()
	t.Setenv("MUSIC_DIR", base)
	for _, d := range []string{"Artist/Album A", "Artist/@eaDir", "Artist/Album A2", "Artist/Album B"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"Artist/Album A/01.flac", "Artist/@eaDir/x.flac", "Artist/Album B/01.flac"} {
		if err := os.WriteFile(filepath.Join(base, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tracks, next := nextAlbumTracks("Artist/Album A/01.flac")
	if next != "Album B" {
		t.Fatalf("next = %q, want Album B", next)
	}
	if len(tracks) != 1 || tracks[0] != "Artist/Album B/01.flac" {
		t.Fatalf("tracks = %v", tracks)
	}
}

func TestNextAlbumRejectsTraversal(t *testing.T) {
	t.Setenv("MUSIC_DIR", t.TempDir())
	for _, p := range []string{"../../etc/passwd", "/etc/passwd", "A/../../../etc/x.flac", ""} {
		if tracks, next := nextAlbumTracks(p); tracks != nil || next != "" {
			t.Errorf("nextAlbumTracks(%q) = %v, %q; want nil", p, tracks, next)
		}
	}
}
