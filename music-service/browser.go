package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// browserAlbums — альбомы по файловой системе (без MPD).
func browserAlbums() []fiber.Map {
	files := musicIndex()
	groups := map[string]int{}
	for _, f := range files {
		groups[filepath.ToSlash(filepath.Dir(f.Path))]++
	}
	out := make([]fiber.Map, 0, len(groups))
	for dir, n := range groups {
		parts := strings.Split(dir, "/")
		artist, album := "Неизвестный", "Без альбома"
		switch {
		case dir == "." || dir == "":
			// файлы в корне библиотеки
		case len(parts) >= 2:
			artist, album = parts[len(parts)-2], parts[len(parts)-1]
		default:
			artist, album = parts[0], parts[0]
		}
		out = append(out, fiber.Map{"artist": artist, "album": album, "dir": dir, "track_count": n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a := strings.ToLower(out[i]["artist"].(string) + " " + out[i]["album"].(string))
		b := strings.ToLower(out[j]["artist"].(string) + " " + out[j]["album"].(string))
		if a != b {
			return a < b
		}
		return out[i]["dir"].(string) < out[j]["dir"].(string)
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

// transcodeExts — форматы, для которых разрешён mp3-транскод.
var transcodeExts = map[string]bool{
	".dsf": true, ".dff": true, ".ape": true, ".wv": true, ".wma": true,
	".aiff": true, ".aif": true,
}

var (
	transcodeSem = make(chan struct{}, 1) // 1 CPU в контейнере
	cacheMu      sync.Mutex
	cacheLocks   = map[string]*sync.Mutex{}

	cacheDirOnce sync.Once
	cacheDirPath string
	cacheDirErr  error
)

// cacheLockFor — один мьютекс на путь в кэше (транскоды и обложки).
func cacheLockFor(key string) *sync.Mutex {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if mu, ok := cacheLocks[key]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	cacheLocks[key] = mu
	return mu
}

// mediaCacheDir — общий кэш транскодов и встроенных обложек.
func mediaCacheDir() (string, error) {
	cacheDirOnce.Do(func() {
		dir := platform.GetEnv("MUSIC_CACHE_DIR", filepath.Join(os.TempDir(), "nas-music-cache"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			cacheDirErr = err
			return
		}
		// Подменённый симлинком каталог кэша не принимаем.
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			cacheDirErr = fmt.Errorf("unsafe media cache dir %q", dir)
			return
		}
		cacheDirPath = dir
	})
	return cacheDirPath, cacheDirErr
}

// usableCacheFile — обычный непустой файл (Lstat: симлинк кэшем не считается).
func usableCacheFile(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// transcodeToMP3 кэширует mp3-транскод. Ключ учитывает mtime+size источника,
// запись атомарная (.part → rename), конкурентные запросы схлопываются.
func transcodeToMP3(src string) (string, error) {
	fi, err := os.Stat(src)
	if err != nil || !fi.Mode().IsRegular() {
		return "", os.ErrNotExist
	}
	dir, err := mediaCacheDir()
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s|%d|%d|q2", src, fi.Size(), fi.ModTime().UnixNano())
	sum := sha256.Sum256([]byte(key))
	out := filepath.Join(dir, "tr_"+hex.EncodeToString(sum[:16])+".mp3")

	if usableCacheFile(out) {
		return out, nil
	}
	mu := cacheLockFor(out)
	mu.Lock()
	defer mu.Unlock()
	if usableCacheFile(out) { // double-check после лока
		return out, nil
	}

	select {
	case transcodeSem <- struct{}{}:
		defer func() { <-transcodeSem }()
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("transcode: busy")
	}

	tmp, err := os.CreateTemp(dir, ".part-*.mp3")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpName)

	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, ff,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-protocol_whitelist", "file", // запрет http/tcp/concat/subfile
		"-i", "file:"+src,
		"-vn", "-map", "0:a:0",
		"-c:a", "libmp3lame", "-q:a", "2", "-f", "mp3",
		tmpName)
	if outb, err := cmd.CombinedOutput(); err != nil {
		log.Printf("transcode %q: %v: %.300s", src, err, outb)
		return "", err
	}
	if !usableCacheFile(tmpName) {
		return "", fmt.Errorf("transcode: empty output")
	}
	if err := os.Rename(tmpName, out); err != nil {
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
		dir, ok := normalizeAlbumDir(c.Query("dir"))
		if !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "dir required"})
		}
		if _, ok := safeMusicDir(dir); !ok {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "album not found"})
		}
		tracks := browserAlbumTracks(dir)
		return c.JSON(fiber.Map{"dir": dir, "album": filepath.Base(dir), "tracks": tracks, "count": len(tracks)})
	})

	// Стриминг файла: range для поддерживаемых форматов, mp3-транскод для остальных.
	app.Get("/browser/stream", func(c fiber.Ctx) error {
		return serveAudioFile(c, c.Query("file"))
	})
	// Алиас из спецификации v1.3: /stream/audio/:id (id = путь файла или индекс).
	app.Get("/stream/audio/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		// Сначала путь: числовые имена файлов не должны перехватываться индексом.
		if _, ok := safeMusicFile(id); ok {
			return serveAudioFile(c, id)
		}
		n, err := strconv.Atoi(id)
		if err != nil {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "file not found"})
		}
		files := musicIndex()
		if n < 0 || n >= len(files) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "file not found"})
		}
		return serveAudioFile(c, files[n].Path)
	})
	app.Get("/stream/audio", func(c fiber.Ctx) error {
		return serveAudioFile(c, c.Query("file"))
	})
}

// serveAudioFile отдаёт файл с Range; для DSD/APE/WV — mp3-транскод.
func serveAudioFile(c fiber.Ctx, rel string) error {
	if rel == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "file required"})
	}
	// 404 и для «вне библиотеки», и для «нет файла» — без oracle.
	path, ok := safeMusicFile(rel)
	if !ok {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	ext := strings.ToLower(filepath.Ext(path))
	c.Set("X-Content-Type-Options", "nosniff")

	if !browserPlayable[ext] {
		if !transcodeExts[ext] {
			return c.Status(fiber.StatusUnsupportedMediaType).JSON(fiber.Map{"error": "unsupported format"})
		}
		mp3, err := transcodeToMP3(path)
		if err != nil {
			log.Printf("transcode %q: %v", path, err)
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "transcode failed"})
		}
		c.Set("Content-Type", "audio/mpeg")
		c.Set("Cache-Control", "private, max-age=86400")
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
	c.Set("Cache-Control", "private, max-age=86400")
	return c.SendFile(path, fiber.SendFile{ByteRange: true})
}
