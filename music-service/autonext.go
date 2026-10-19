package main

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

// nextAlbumTracks ищет следующий по алфавиту альбом (папку) рядом с текущим
// и возвращает его аудиофайлы (пути относительно MUSIC_DIR).
func nextAlbumTracks(currentFile string) ([]string, string) {
	base := platform.GetEnv("MUSIC_DIR", "/mnt/nas/root/FuzzyHD/Music")
	if filepath.IsAbs(currentFile) {
		base = ""
	}
	dir := filepath.Dir(currentFile)
	parent := filepath.Dir(dir)

	entries, err := os.ReadDir(filepath.Join(base, parent))
	if err != nil {
		return nil, ""
	}
	names := []string{}
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	cur := filepath.Base(dir)
	idx := -1
	for i, n := range names {
		if n == cur {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(names) {
		return nil, ""
	}
	next := names[idx+1]
	files, err := os.ReadDir(filepath.Join(base, parent, next))
	if err != nil {
		return nil, ""
	}
	out := []string{}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := strings.ToLower(f.Name())
		if strings.HasPrefix(name, ".") {
			continue
		}
		if audioExtsSmart[strings.ToLower(filepath.Ext(name))] {
			out = append(out, filepath.ToSlash(filepath.Join(parent, next, f.Name())))
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, ""
	}
	return out, next
}

// autoNextAlbum переключает MPD на следующий альбом.
func autoNextAlbum(m *mpdClient, currentFile string) {
	tracks, next := nextAlbumTracks(currentFile)
	if len(tracks) == 0 {
		log.Printf("🏁 Автопереход: следующего альбома нет")
		return
	}
	_ = m.exec("clear")
	for _, t := range tracks {
		_ = m.exec("add", t)
	}
	_ = m.exec("play")
	log.Printf("▶️ Автопереход: %s → %s (%d треков)", filepath.Dir(currentFile), next, len(tracks))
}
