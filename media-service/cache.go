package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

const (
	photoCacheKindThumb = "thumb"
	photoCacheKindFull  = "full"

	photoCacheVersion = "v2"

	photoCacheMinSize = 64
	photoCacheMaxSize = 4096

	photoFFmpegTimeout = 20 * time.Second
	photoFFmpegWorkers = 2

	photoFailTTL = time.Minute
)

// photoCacheDir — локальный кэш на SSD (в compose монтируется named volume,
// поэтому переживает пересоздание контейнера; на NAS ничего не пишем).
var photoCacheDir = platform.GetEnv("PHOTO_CACHE_DIR", "/var/cache/nas-photos")

var (
	errPhotoUnavailable = errors.New("image unavailable")
	errPhotoUnsafePath  = errors.New("photo path outside media root")

	// photoCacheLocks защищает от одновременной генерации одной и той же картинки.
	photoCacheLocks = &photoLockMap{m: make(map[string]*photoKeyLock)}

	// clearPhotoCache берёт Lock, генерация — RLock.
	photoCacheClearMu sync.RWMutex

	// Ограничитель одновременных ffmpeg (NAS = 1–2 CPU).
	ffmpegSem = make(chan struct{}, photoFFmpegWorkers)

	// Отрицательный кэш: не долбить ffmpeg по заведомо битому файлу.
	photoFailures = struct {
		sync.Mutex
		m map[string]time.Time
	}{m: make(map[string]time.Time)}
)

type photoCacheMeta struct {
	OrigSize  int64 `json:"orig_size"`
	OrigMtime int64 `json:"orig_mtime_ns"`
}

type photoKeyLock struct {
	mu   sync.Mutex
	refs int
}

type photoLockMap struct {
	sync.Mutex
	m map[string]*photoKeyLock
}

func initPhotoCache() error {
	dir := filepath.Clean(photoCacheDir)
	if !filepath.IsAbs(dir) || dir == string(filepath.Separator) {
		return fmt.Errorf("photo cache: invalid PHOTO_CACHE_DIR %q", photoCacheDir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("photo cache: mkdir: %w", err)
	}

	probe := filepath.Join(dir, ".writable")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("photo cache: not writable: %w", err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}

// photoRoot — корень фото-библиотеки после резолва симлинков.
func photoRoot() string {
	root := filepath.Clean(platform.GetEnv("MEDIA_PHOTOS_DIR", "/photos"))
	if r, err := filepath.EvalSymlinks(root); err == nil {
		return r
	}
	return root
}

// canonicalPhotoRel убирает alias-варианты одного файла:
// "a.jpg", "./a.jpg", "a//a.jpg", "/a.jpg" → один ключ.
func canonicalPhotoRel(rel string) string {
	return filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(filepath.ToSlash(rel), "/")))
}

func validPhotoKind(kind string) bool {
	return kind == photoCacheKindThumb || kind == photoCacheKindFull
}

func validPhotoSize(size int) bool {
	return size >= photoCacheMinSize && size <= photoCacheMaxSize
}

func photoCacheKey(rel string, size int, kind string) (string, error) {
	if !validPhotoKind(kind) {
		return "", errors.New("photo cache: invalid kind")
	}
	if !validPhotoSize(size) {
		return "", errors.New("photo cache: invalid size")
	}

	h := sha256.New()
	for _, part := range []string{
		filepath.Clean(photoCacheDir),
		canonicalPhotoRel(rel),
		kind,
		strconv.Itoa(size),
		photoCacheVersion,
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

func photoCachePath(rel string, size int, kind string) (string, error) {
	key, err := photoCacheKey(rel, size, kind)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(photoCacheDir), key+".jpg"), nil
}

// lockPhotoCache — per-key mutex с подсчётом ссылок: запись в map удаляется,
// когда последний ожидающий отпустил ключ (иначе map течёт по памяти).
func lockPhotoCache(key string) func() {
	photoCacheLocks.Lock()
	l := photoCacheLocks.m[key]
	if l == nil {
		l = &photoKeyLock{}
		photoCacheLocks.m[key] = l
	}
	l.refs++
	photoCacheLocks.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()

		photoCacheLocks.Lock()
		l.refs--
		if l.refs == 0 {
			delete(photoCacheLocks.m, key)
		}
		photoCacheLocks.Unlock()
	}
}

func readPhotoCacheMeta(p string) (photoCacheMeta, bool) {
	b, err := os.ReadFile(p + ".meta")
	if err != nil {
		return photoCacheMeta{}, false
	}
	var m photoCacheMeta
	if json.Unmarshal(b, &m) != nil {
		return photoCacheMeta{}, false
	}
	return m, true
}

// cachedPhoto — чистый read-path: ничего не удаляет (удаление делает владелец ключа).
func cachedPhoto(rel string, size int, kind, origPath string) (string, bool) {
	p, err := photoCachePath(rel, size, kind)
	if err != nil {
		return "", false
	}

	ci, err := os.Lstat(p)
	if err != nil || !ci.Mode().IsRegular() || ci.Size() < 3 {
		return "", false
	}

	oi, err := os.Stat(origPath)
	if err != nil || !oi.Mode().IsRegular() {
		return "", false
	}

	m, ok := readPhotoCacheMeta(p)
	if !ok {
		return "", false
	}
	if m.OrigSize != oi.Size() || m.OrigMtime != oi.ModTime().UnixNano() {
		return "", false
	}
	return p, true
}

// saveCachedPhoto пишет атомарно: уникальный tmp + fsync + rename.
// Валидность определяется sidecar-метаданными (size + mtime оригинала).
func saveCachedPhoto(rel string, size int, kind, origPath string, data []byte) (string, error) {
	if len(data) < 3 || data[0] != 0xFF || data[1] != 0xD8 || data[2] != 0xFF {
		return "", errors.New("photo cache: not a JPEG")
	}

	p, err := photoCachePath(rel, size, kind)
	if err != nil {
		return "", err
	}

	oi, err := os.Stat(origPath)
	if err != nil {
		return "", fmt.Errorf("photo cache: stat original: %w", err)
	}
	if !oi.Mode().IsRegular() {
		return "", errPhotoUnsafePath
	}

	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	f, err := os.CreateTemp(dir, "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()

	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + ".meta")
	}

	if _, err := f.Write(data); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + ".meta")
		return "", err
	}

	meta, err := json.Marshal(photoCacheMeta{
		OrigSize:  oi.Size(),
		OrigMtime: oi.ModTime().UnixNano(),
	})
	if err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + ".meta")
		return "", err
	}
	if err := os.WriteFile(tmp+".meta", meta, 0o600); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + ".meta")
		return "", err
	}

	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + ".meta")
		return "", err
	}
	if err := os.Rename(tmp+".meta", p+".meta"); err != nil {
		_ = os.Remove(p)
		return "", err
	}
	syncDir(dir)
	return p, nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// validatedOrigPath — defense in depth: даже если вызывающий забыл photoAbsPath,
// ffmpeg не получит путь вне MEDIA_PHOTOS_DIR (включая симлинк-побег).
func validatedOrigPath(origPath string) (string, error) {
	if !filepath.IsAbs(origPath) {
		return "", errPhotoUnsafePath
	}

	root := photoRoot()
	if root == string(filepath.Separator) {
		return "", errPhotoUnsafePath
	}

	real, err := filepath.EvalSymlinks(origPath)
	if err != nil {
		return "", err
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", errPhotoUnsafePath
	}

	fi, err := os.Stat(real)
	if err != nil || !fi.Mode().IsRegular() {
		return "", errPhotoUnsafePath
	}
	return real, nil
}

func photoRecentlyFailed(key string) bool {
	photoFailures.Lock()
	defer photoFailures.Unlock()

	until, ok := photoFailures.m[key]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(photoFailures.m, key)
		return false
	}
	return true
}

func markPhotoFailure(key string) {
	photoFailures.Lock()
	photoFailures.m[key] = time.Now().Add(photoFailTTL)
	photoFailures.Unlock()
}

func clearPhotoFailure(key string) {
	photoFailures.Lock()
	delete(photoFailures.m, key)
	photoFailures.Unlock()
}

func runFFmpeg(origPath string, size int, isVideo, seek bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), photoFFmpegTimeout)
	defer cancel()

	select {
	case ffmpegSem <- struct{}{}:
		defer func() { <-ffmpegSem }()
	case <-ctx.Done():
		return nil, errors.New("ffmpeg: queue timeout")
	}

	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		// Блокирует http/tcp/concat (SSRF из плейлиста).
		"-protocol_whitelist", "file",
	}
	if isVideo && seek {
		args = append(args, "-ss", "1")
	}
	args = append(args,
		"-i", origPath,
		"-frames:v", "1",
		"-threads", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", size, size),
		"-q:v", "3",
		"-f", "image2pipe", "-vcodec", "mjpeg", "-",
	)

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second

	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, errors.New("ffmpeg: timeout")
	}
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %v: %s", err, truncatePhotoErr(stderr.String()))
	}
	if len(out) == 0 {
		return nil, errors.New("ffmpeg: empty output")
	}
	return out, nil
}

func truncatePhotoErr(s string) string {
	s = strings.TrimSpace(s)
	const max = 512
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ensurePhotoCache возвращает путь к готовому JPEG (thumb/full), генерируя его
// через ffmpeg при необходимости. isVideo — взять кадр на 1-й секунде.
func ensurePhotoCache(rel string, size int, kind, origPath string, isVideo bool) (string, error) {
	realPath, err := validatedOrigPath(origPath)
	if err != nil {
		return "", err
	}

	key, err := photoCacheKey(rel, size, kind)
	if err != nil {
		return "", err
	}

	photoCacheClearMu.RLock()
	defer photoCacheClearMu.RUnlock()

	if p, ok := cachedPhoto(rel, size, kind, realPath); ok {
		return p, nil
	}
	if photoRecentlyFailed(key) {
		return "", errPhotoUnavailable
	}

	unlock := lockPhotoCache(key)
	defer unlock()

	// Пока ждали лок, другой запрос мог уже сгенерировать кэш.
	if p, ok := cachedPhoto(rel, size, kind, realPath); ok {
		return p, nil
	}
	if photoRecentlyFailed(key) {
		return "", errPhotoUnavailable
	}

	out, err := runFFmpeg(realPath, size, isVideo, true)
	// Видео короче 1 c: seek на 1-ю секунду даёт пустой кадр — пробуем с 0.
	if isVideo && (err != nil || len(out) == 0) {
		out, err = runFFmpeg(realPath, size, false, false)
	}
	if err != nil {
		markPhotoFailure(key)
		return "", errPhotoUnavailable
	}

	saved, err := saveCachedPhoto(rel, size, kind, realPath, out)
	if err != nil {
		markPhotoFailure(key)
		log.Printf("photo cache: save %s: %v", filepath.Base(realPath), err)
		return "", errPhotoUnavailable
	}
	clearPhotoFailure(key)
	return saved, nil
}

// clearPhotoCache удаляет содержимое кэша, не трогая саму точку монтирования.
func clearPhotoCache() error {
	photoCacheClearMu.Lock()
	defer photoCacheClearMu.Unlock()

	dir := filepath.Clean(photoCacheDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return initPhotoCache()
		}
		return err
	}

	var errs []error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}

	photoFailures.Lock()
	photoFailures.m = make(map[string]time.Time)
	photoFailures.Unlock()

	return errors.Join(errs...)
}
