package main

import (
	"crypto/sha1"
	"encoding/hex"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
)

// audioMode — "mpd" (Hi-Res через MPD) или "browser" (HTML5 + ffmpeg).
var (
	audioMode   = "browser"
	audioModeMu sync.RWMutex
)

func detectAudioMode() {
	mode := "browser"
	if m, err := getMPD(); err == nil {
		if _, err := m.raw("ping"); err == nil {
			mode = "mpd"
		}
	}
	audioModeMu.Lock()
	audioMode = mode
	audioModeMu.Unlock()
	if mode == "mpd" {
		log.Printf("🎵 Audio mode: MPD (Hi-Res)")
	} else {
		log.Printf("🎵 Audio mode: Browser (HTML5 + transcode)")
	}
}

// startAudioModeWatcher проверяет MPD в фоне: без блокировки UI и старта.
func startAudioModeWatcher() {
	go func() {
		detectAudioMode()
		t := time.NewTicker(2 * time.Minute)
		for range t.C {
			detectAudioMode()
		}
	}()
}

func currentAudioMode() string {
	audioModeMu.RLock()
	defer audioModeMu.RUnlock()
	return audioMode
}

// safeMusicPath проверяет, что путь не выходит за MUSIC_DIR.
func safeMusicPath(rel string) (string, bool) {
	base := filepath.Clean(platform.GetEnv("MUSIC_DIR", "/mnt/nas/root/FuzzyHD/Music"))
	clean := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return "", false
	}
	return clean, true
}

// browserAlbums — альбомы по файловой системе (без MPD).
func browserAlbums() []fiber.Map {
	files := musicIndex()
	groups := map[string]int{}
	for _, f := range files {
		groups[filepath.ToSlash(filepath.Dir(f.Path))]++
	}
	out := []fiber.Map{}
	for dir, n := range groups {
		parts := strings.Split(dir, "/")
		artist := parts[0]
		if artist == "" || artist == "." {
			artist = "Неизвестный"
		}
		album := parts[len(parts)-1]
		out = append(out, fiber.Map{"artist": artist, "album": album, "dir": dir, "track_count": n})
	}
	sort.Slice(out, func(i, j int) bool {
		a := strings.ToLower(out[i]["artist"].(string) + " " + out[i]["album"].(string))
		b := strings.ToLower(out[j]["artist"].(string) + " " + out[j]["album"].(string))
		return a < b
	})
	return out
}

func browserAlbumTracks(dir string) []fiber.Map {
	out := []fiber.Map{}
	for _, f := range musicIndex() {
		d := filepath.ToSlash(filepath.Dir(f.Path))
		if d != dir {
			continue
		}
		title := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		out = append(out, fiber.Map{"file": f.Path, "title": title, "album": filepath.Base(dir), "dir": dir})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["file"].(string) < out[j]["file"].(string)
	})
	return out
}

// browserPlayable — форматы, которые браузер играет напрямую.
var browserPlayable = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".aac": true,
	".ogg": true, ".oga": true, ".opus": true, ".wav": true,
}

// transcodeToMP3 кэширует mp3-транскод для DSD/APE/WV и т.п.
func transcodeToMP3(src string) (string, error) {
	h := sha1.Sum([]byte(src))
	out := filepath.Join(os.TempDir(), "nas_tr_"+hex.EncodeToString(h[:8])+".mp3")
	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		return out, nil
	}
	cmd := exec.Command("ffmpeg", "-y", "-i", src, "-vn", "-c:a", "libmp3lame", "-q:a", "2", out)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out, nil
}

func browserRoutes(app *fiber.App) {
	// Текущий режим аудио (свежая проверка MPD).
	app.Get("/audio/mode", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"mode": currentAudioMode()})
	})

	app.Get("/browser/albums", func(c fiber.Ctx) error {
		al := browserAlbums()
		return c.JSON(fiber.Map{"albums": al, "count": len(al)})
	})

	app.Get("/browser/album", func(c fiber.Ctx) error {
		dir := c.Query("dir")
		if dir == "" {
			return c.Status(400).JSON(fiber.Map{"error": "dir required"})
		}
		if _, ok := safeMusicPath(dir); !ok {
			return c.Status(403).JSON(fiber.Map{"error": "forbidden"})
		}
		tracks := browserAlbumTracks(dir)
		return c.JSON(fiber.Map{"dir": dir, "album": filepath.Base(dir), "tracks": tracks, "count": len(tracks)})
	})

	// Стриминг файла: range для поддерживаемых форматов, mp3-транскод для остальных.
	app.Get("/browser/stream", func(c fiber.Ctx) error {
		return serveAudioFile(c, c.Query("file"))
	})
	// Алиас из спецификации v1.3: /stream/audio/:id (id = индекс файла или путь).
	app.Get("/stream/audio/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		if n, err := strconv.Atoi(id); err == nil {
			files := musicIndex()
			if n < 0 || n >= len(files) {
				return c.Status(404).JSON(fiber.Map{"error": "file not found"})
			}
			return serveAudioFile(c, files[n].Path)
		}
		return serveAudioFile(c, id)
	})
	app.Get("/stream/audio", func(c fiber.Ctx) error {
		return serveAudioFile(c, c.Query("file"))
	})
}

// serveAudioFile отдаёт файл с Range; для DSD/APE/WV — mp3-транскод.
func serveAudioFile(c fiber.Ctx, rel string) error {
	if rel == "" {
		return c.Status(400).JSON(fiber.Map{"error": "file required"})
	}
	path, ok := safeMusicPath(rel)
	if !ok {
		return c.Status(403).JSON(fiber.Map{"error": "forbidden"})
	}
	if _, err := os.Stat(path); err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "not found"})
	}
	ext := strings.ToLower(filepath.Ext(path))
	if !browserPlayable[ext] {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "ffmpeg not available"})
		}
		mp3, err := transcodeToMP3(path)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "transcode: " + err.Error()})
		}
		c.Set("Content-Type", "audio/mpeg")
		c.Set("Cache-Control", "public, max-age=86400")
		return c.SendFile(mp3, fiber.SendFile{ByteRange: true})
	}
	switch ext {
	case ".mp3":
		c.Set("Content-Type", "audio/mpeg")
	case ".flac":
		c.Set("Content-Type", "audio/flac")
	case ".wav":
		c.Set("Content-Type", "audio/wav")
	case ".ogg", ".oga", ".opus":
		c.Set("Content-Type", "audio/ogg")
	case ".m4a", ".aac":
		c.Set("Content-Type", "audio/mp4")
	}
	c.Set("Cache-Control", "public, max-age=86400")
	return c.SendFile(path, fiber.SendFile{ByteRange: true})
}
