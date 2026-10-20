package main

import (
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// lastManualStop — время последнего ручного POST /stop.
// Автопереход не должен срабатывать сразу после действия пользователя.
var lastManualStop atomic.Int64

func markManualStop() { lastManualStop.Store(time.Now().UnixNano()) }

func autoNextAllowed() bool {
	ts := lastManualStop.Load()
	return ts == 0 || time.Since(time.Unix(0, ts)) > 20*time.Second
}

// nextAlbumTracks ищет следующий по алфавиту играбельный альбом рядом с текущим.
// Возвращает пути относительно MUSIC_DIR и имя альбома.
func nextAlbumTracks(currentFile string) ([]string, string) {
	rel, ok := relMusicPath(currentFile)
	if !ok {
		log.Printf("autonext: %q вне MUSIC_DIR", currentFile)
		return nil, ""
	}
	dir := path.Dir(rel)
	if dir == "." {
		return nil, "" // альбом в корне библиотеки не поддерживаем
	}
	parent := path.Dir(dir)
	if parent == "." {
		parent = ""
	}

	absParent, ok := safeMusicDirOrRoot(parent)
	if !ok {
		log.Printf("autonext: каталог %q недоступен", parent)
		return nil, ""
	}
	entries, err := os.ReadDir(absParent)
	if err != nil {
		log.Printf("autonext: read %q: %v", parent, err)
		return nil, ""
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if isIgnoredAlbumDir(e.Name()) {
			continue
		}
		if e.IsDir() {
			names = append(names, e.Name())
			continue
		}
		// симлинк-каталог тоже кандидат: containment проверит albumAudioFiles
		if e.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(absParent, e.Name())); err == nil && fi.IsDir() {
				names = append(names, e.Name())
			}
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		li, lj := strings.ToLower(names[i]), strings.ToLower(names[j])
		if li != lj {
			return li < lj
		}
		return names[i] < names[j]
	})

	cur := path.Base(dir)
	idx := -1
	for i, n := range names {
		if n == cur {
			idx = i
			break
		}
	}
	if idx < 0 { // case-insensitive ФС (SMB/exFAT)
		for i, n := range names {
			if strings.EqualFold(n, cur) {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return nil, ""
	}

	for _, cand := range names[idx+1:] {
		relDir := cand
		if parent != "" {
			relDir = path.Join(parent, cand)
		}
		tracks, err := albumAudioFiles(relDir)
		if err != nil {
			log.Printf("autonext: read album %q: %v", relDir, err)
			continue
		}
		if len(tracks) == 0 {
			continue // пустой/служебный каталог — ищем дальше
		}
		return tracks, cand
	}
	return nil, ""
}

// autoNextAlbum переключает MPD на следующий альбом.
// Ревалидация и замена очереди выполняются под одним queueMu.
func autoNextAlbum(m *mpdClient, currentFile string) {
	if currentFile == "" {
		return
	}
	queueMu.Lock()
	defer queueMu.Unlock()

	// За время поллинга пользователь мог поставить другую музыку.
	if !autoNextAllowed() {
		return
	}
	st := fieldsToMap(m.fields("status"))
	if state, _ := st["state"].(string); state != "stop" {
		return
	}
	if st["repeat"] == "1" || st["single"] == "1" {
		return
	}
	cur := fieldsToMap(m.fields("currentsong"))
	if f, _ := cur["file"].(string); f != "" && f != currentFile {
		return
	}

	tracks, next := nextAlbumTracks(currentFile)
	if len(tracks) == 0 {
		log.Printf("🏁 Автопереход: следующего альбома нет (после %q)", path.Dir(currentFile))
		return
	}
	added, err := replaceQueueAndPlayLocked(m, tracks)
	if err != nil {
		log.Printf("⚠️ Автопереход %q → %q: %v", path.Dir(currentFile), next, err)
		return
	}
	log.Printf("▶️ Автопереход: %q → %q (%d треков)", path.Dir(currentFile), next, added)
}
