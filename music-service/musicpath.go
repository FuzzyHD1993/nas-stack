package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

const defaultMusicDir = "/mnt/nas/root/FuzzyHD/Music"

var musicRootCache struct {
	sync.Mutex
	env  string
	real string
}

// musicRoot возвращает реальный (со снятыми симлинками) корень библиотеки.
func musicRoot() (string, bool) {
	env := platform.GetEnv("MUSIC_DIR", defaultMusicDir)

	musicRootCache.Lock()
	defer musicRootCache.Unlock()
	if musicRootCache.env == env && musicRootCache.real != "" {
		return musicRootCache.real, true
	}
	abs, err := filepath.Abs(env)
	if err != nil {
		return "", false
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", false
	}
	musicRootCache.env, musicRootCache.real = env, real
	return real, true
}

// withinRoot — p лежит внутри root (или равен root). Через filepath.Rel:
// корректно работает для root="/" и не зависит от строковых префиксов.
func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// safeMusicPath — лексическая проверка вхождения в MUSIC_DIR.
// Существование файла не требуется; если путь существует, симлинки
// резолвятся и цель обязана остаться внутри корня.
func safeMusicPath(rel string) (string, bool) { return safeMusicPathWith(rel, false) }

// safeMusicFile — путь существует, это обычный файл внутри MUSIC_DIR.
func safeMusicFile(rel string) (string, bool) {
	p, ok := safeMusicPathWith(rel, true)
	if !ok {
		return "", false
	}
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return p, true
}

// safeMusicDir — путь существует и это каталог внутри MUSIC_DIR.
func safeMusicDir(rel string) (string, bool) {
	p, ok := safeMusicPathWith(rel, true)
	if !ok {
		return "", false
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		return "", false
	}
	return p, true
}

// safeMusicDirOrRoot — как safeMusicDir, но "" и "." означают корень библиотеки.
func safeMusicDirOrRoot(rel string) (string, bool) {
	if rel == "" || rel == "." || rel == "/" {
		return musicRoot()
	}
	return safeMusicDir(rel)
}

func safeMusicPathWith(rel string, mustExist bool) (string, bool) {
	root, ok := musicRoot()
	if !ok || rel == "" || strings.ContainsRune(rel, 0) {
		return "", false
	}
	p := filepath.FromSlash(rel)
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	clean := filepath.Clean(p)
	if !withinRoot(root, clean) {
		return "", false
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if mustExist {
			return "", false
		}
		return clean, true // ещё не существует, но лексически внутри root
	}
	if !withinRoot(root, real) {
		return "", false
	}
	return real, true
}

// relMusicPath приводит путь из MPD (относительный URI или абсолютный путь
// внутри библиотеки) к пути относительно MUSIC_DIR. Всё вне корня — отказ.
func relMusicPath(p string) (string, bool) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", false
	}
	root, ok := musicRoot()
	if !ok {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	if filepath.IsAbs(clean) {
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			return "", false
		}
		clean = rel
	}
	if clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.IsAbs(clean) {
		return "", false
	}
	return filepath.ToSlash(clean), true
}

// normalizeAlbumDir канонизирует dir из query-параметра.
func normalizeAlbumDir(rel string) (string, bool) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return "", false
	}
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// isIgnoredAlbumDir — служебные каталоги NAS, которые нельзя считать альбомом.
func isIgnoredAlbumDir(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") ||
		strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#") {
		return true
	}
	switch strings.ToLower(name) {
	case "$recycle.bin", "lost+found", "recycler", "system volume information":
		return true
	}
	return false
}

// albumAudioFiles возвращает аудиофайлы каталога (пути относительно MUSIC_DIR).
// Симлинки и имена с управляющими символами отбрасываются.
func albumAudioFiles(relDir string) ([]string, error) {
	abs, ok := safeMusicDir(relDir)
	if !ok {
		return nil, fmt.Errorf("musicpath: %q вне MUSIC_DIR", relDir)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.ContainsAny(name, "\r\n\x00") {
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		if !audioExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		out = append(out, path.Join(relDir, name))
	}
	sort.Strings(out)
	return out, nil
}

// safeAlbumDir — реальный каталог альбома по MPD-пути файла; "" если путь вне
// библиотеки или каталог недоступен.
func safeAlbumDir(mpdFile string) string {
	rel, ok := relMusicPath(mpdFile)
	if !ok {
		return ""
	}
	dir := path.Dir(rel)
	if dir == "." {
		return ""
	}
	abs, ok := safeMusicDir(dir)
	if !ok {
		return ""
	}
	return abs
}
