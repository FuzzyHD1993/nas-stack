package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindCoverInDirAnyImage(t *testing.T) {
	dir := t.TempDir()
	anyImg := filepath.Join(dir, "Artist - Album.jpg")
	if err := os.WriteFile(anyImg, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findCoverInDir(dir)
	if err != nil || got != anyImg {
		t.Fatalf("findCoverInDir = %q, %v; want %q", got, err, anyImg)
	}
}

func TestFindCoverInDirNamedWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Artist - Album.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(dir, "cover.png")
	if err := os.WriteFile(named, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := findCoverInDir(dir)
	if got != named {
		t.Errorf("named cover = %q, want %q", got, named)
	}
}

func TestFindCoverInDirRussian(t *testing.T) {
	dir := t.TempDir()
	ru := filepath.Join(dir, "Обложка.jpg")
	if err := os.WriteFile(ru, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := findCoverInDir(dir)
	if err != nil || got != ru {
		t.Fatalf("russian cover = %q, %v; want %q", got, err, ru)
	}
}

func TestFindCoverInDirNone(t *testing.T) {
	if _, err := findCoverInDir(t.TempDir()); err == nil {
		t.Error("expected error for dir without images")
	}
}
