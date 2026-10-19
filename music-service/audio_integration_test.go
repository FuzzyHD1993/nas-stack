//go:build integration

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseAudioInfoIntegration проверяет разбор реального ответа MPD
// (flac:96000:24:2) без запуска MPD.
func TestParseAudioInfoIntegration(t *testing.T) {
	old := bitPerfectEnabled
	bitPerfectEnabled = false
	defer func() { bitPerfectEnabled = old }()

	info := parseAudioInfo("track.flac", map[string]string{
		"audio":   "96000:24:2",
		"bitrate": "4608",
	})
	if info.Codec != "flac" {
		t.Errorf("codec = %q, want flac", info.Codec)
	}
	if info.SampleRate != 96000 || info.Bits != 24 || info.Channels != 2 {
		t.Errorf("audio = %d/%d/%d, want 96000/24/2", info.SampleRate, info.Bits, info.Channels)
	}
	if info.BitrateKbps != 4608 {
		t.Errorf("bitrate = %d, want 4608", info.BitrateKbps)
	}
	if info.Label != "FLAC 24bit/96.0kHz" {
		t.Errorf("label = %q", info.Label)
	}
}

// TestFindCoverInDirIntegration проверяет рекурсивный поиск обложки —
// сценарий реального альбома с папкой Scans/.
func TestFindCoverInDirIntegration(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "Scans")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cover := filepath.Join(sub, "cover-max.jpg")
	if err := os.WriteFile(cover, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01.flac"), []byte("flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := findCoverInDir(dir)
	if err != nil {
		t.Fatalf("findCoverInDir: %v", err)
	}
	if got != cover {
		t.Errorf("findCoverInDir = %q, want %q", got, cover)
	}
}
