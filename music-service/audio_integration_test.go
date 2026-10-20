//go:build integration

package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProbeSourceAudioFFprobe — реальный вызов ffprobe на сгенерированном файле.
func TestProbeSourceAudioFFprobe(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	dir := t.TempDir()
	t.Setenv("MUSIC_DIR", dir)
	src := filepath.Join(dir, "t.flac")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi",
		"-i", "anullsrc=r=96000:cl=stereo", "-c:a", "flac", "-sample_fmt", "s32",
		"-t", "1", "-y", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot generate fixture: %v (%s)", err, out)
	}

	rate, bits, codec := probeSourceAudio("t.flac")
	if rate != 96000 {
		t.Errorf("rate = %d, want 96000", rate)
	}
	if bits == 0 {
		t.Errorf("bits = 0, want > 0")
	}
	if codec != "flac" {
		t.Errorf("codec = %q, want flac", codec)
	}
}

// TestProbeSourceAudioRejectsEscape — ffprobe не должен запускаться вовсе.
func TestProbeSourceAudioRejectsEscape(t *testing.T) {
	t.Setenv("MUSIC_DIR", t.TempDir())
	for _, bad := range []string{
		"", "../../etc/passwd", "/etc/passwd", "-f lavfi",
		"file:///etc/shadow", "http://127.0.0.1:6379/", "concat:http://evil/x",
	} {
		t.Run(bad, func(t *testing.T) {
			if r, b, c := probeSourceAudio(bad); r != 0 || b != 0 || c != "" {
				t.Fatalf("probeSourceAudio(%q) = %d/%d/%q, want zeros", bad, r, b, c)
			}
		})
	}
}

// TestParseAudioInfoIntegration проверяет разбор реального ответа MPD
// (flac:96000:24:2) без запуска MPD.
func TestParseAudioInfoIntegration(t *testing.T) {
	old := bitPerfectEnabled.Load()
	bitPerfectEnabled.Store(false)
	defer bitPerfectEnabled.Store(old)

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
