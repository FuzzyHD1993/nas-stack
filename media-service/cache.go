package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

// photoCacheDir — локальный кэш на SSD (в compose монтируется named volume,
// поэтому переживает пересоздание контейнера; на NAS ничего не пишем).
var photoCacheDir = platform.GetEnv("PHOTO_CACHE_DIR", "/var/cache/nas-photos")

// photoCacheLocks защищает от одновременной генерации одной и той же картинки.
var photoCacheLocks sync.Map // key -> *sync.Mutex

func initPhotoCache() {
	if err := os.MkdirAll(photoCacheDir, 0o755); err != nil {
		fmt.Printf("⚠️ photo cache: %v\n", err)
	}
}

func photoCacheKey(rel string, size int, kind string) string {
	h := sha256.Sum256([]byte(rel))
	return hex.EncodeToString(h[:16]) + "_" + kind + "_" + strconv.Itoa(size)
}

func photoCachePath(rel string, size int, kind string) string {
	return filepath.Join(photoCacheDir, photoCacheKey(rel, size, kind)+".jpg")
}

// lockPhotoCache — per-key mutex: одинаковые миниатюры не ресайзятся параллельно.
func lockPhotoCache(key string) func() {
	v, _ := photoCacheLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// cachedPhoto возвращает путь к кэшу, если он есть и не старше оригинала.
func cachedPhoto(rel string, size int, kind, origPath string) (string, bool) {
	p := photoCachePath(rel, size, kind)
	ci, err := os.Stat(p)
	if err != nil || ci.Size() == 0 {
		return "", false
	}
	oi, err := os.Stat(origPath)
	if err != nil {
		return "", false
	}
	// mtime оригинала новее кэша → фото изменилось, кэш невалиден.
	if ci.ModTime().Before(oi.ModTime()) {
		_ = os.Remove(p)
		return "", false
	}
	return p, true
}

// saveCachedPhoto пишет атомарно (tmp + rename) и ставит mtime оригинала.
func saveCachedPhoto(rel string, size int, kind, origPath string, data []byte) (string, error) {
	p := photoCachePath(rel, size, kind)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if oi, err := os.Stat(origPath); err == nil {
		_ = os.Chtimes(tmp, time.Now(), oi.ModTime())
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return p, nil
}

// ensurePhotoCache возвращает путь к готовому JPEG (thumb/full), генерируя его
// через ffmpeg при необходимости. isVideo — взять кадр на 1-й секунде.
func ensurePhotoCache(rel string, size int, kind, origPath string, isVideo bool) (string, error) {
	if p, ok := cachedPhoto(rel, size, kind, origPath); ok {
		return p, nil
	}
	unlock := lockPhotoCache(photoCacheKey(rel, size, kind))
	defer unlock()
	// Пока ждали лок, другой запрос мог уже сгенерировать кэш.
	if p, ok := cachedPhoto(rel, size, kind, origPath); ok {
		return p, nil
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if isVideo {
		args = append(args, "-ss", "1")
	}
	args = append(args,
		"-i", origPath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", size, size),
		"-q:v", "3",
		"-f", "image2pipe", "-vcodec", "mjpeg", "-",
	)
	out, err := exec.Command("ffmpeg", args...).Output()
	if err != nil {
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("ffmpeg: empty output")
	}
	return saveCachedPhoto(rel, size, kind, origPath, out)
}

// clearPhotoCache удаляет содержимое кэша, не трогая саму точку монтирования.
func clearPhotoCache() error {
	entries, err := os.ReadDir(photoCacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			initPhotoCache()
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(photoCacheDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
