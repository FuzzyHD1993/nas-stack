package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withPhotoCache(t *testing.T) string {
	t.Helper()
	old := photoCacheDir
	photoCacheDir = t.TempDir()
	initPhotoCache()
	t.Cleanup(func() { photoCacheDir = old })
	return photoCacheDir
}

func TestPhotoCacheInvalidationByMtime(t *testing.T) {
	withPhotoCache(t)
	origDir := t.TempDir()
	orig := filepath.Join(origDir, "photo.jpg")
	if err := os.WriteFile(orig, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(orig, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := saveCachedPhoto("a/photo.jpg", 400, "thumb", orig, []byte("thumb")); err != nil {
		t.Fatal(err)
	}
	if p, ok := cachedPhoto("a/photo.jpg", 400, "thumb", orig); !ok || p == "" {
		t.Fatal("fresh cache not found")
	}

	// Оригинал изменился — кэш должен инвалидироваться.
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(orig, future, future); err != nil {
		t.Fatal(err)
	}
	if _, ok := cachedPhoto("a/photo.jpg", 400, "thumb", orig); ok {
		t.Error("stale cache accepted after mtime change")
	}
	if _, err := os.Stat(photoCachePath("a/photo.jpg", 400, "thumb")); !os.IsNotExist(err) {
		t.Error("stale cache file was not removed")
	}
}

func TestPhotoCacheKeysDiffer(t *testing.T) {
	a := photoCachePath("x.jpg", 400, "thumb")
	b := photoCachePath("x.jpg", 800, "thumb")
	c := photoCachePath("x.jpg", 1200, "full")
	if a == b || a == c || b == c {
		t.Errorf("cache paths must differ: %q %q %q", a, b, c)
	}
}

func TestClearPhotoCache(t *testing.T) {
	dir := withPhotoCache(t)
	if err := os.WriteFile(filepath.Join(dir, "x.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := clearPhotoCache(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cache not cleared: %d entries", len(entries))
	}
}
