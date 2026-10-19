package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

// coverNames — приоритетный список имён файлов обложек.
var coverNames = []string{
	"cover.jpg", "cover.jpeg", "cover.png", "cover.webp",
	"Cover.jpg", "Cover.jpeg", "Cover.png", "Cover.webp",
	"folder.jpg", "folder.jpeg", "folder.png", "folder.webp",
	"Folder.jpg", "Folder.png",
	"front.jpg", "front.png", "Front.jpg", "Front.png",
	"album.jpg", "album.png", "Album.jpg", "Album.png",
	"albumart.jpg", "albumart.png", "AlbumArt.jpg", "AlbumArt.png",
	"artwork.jpg", "artwork.png", "Artwork.jpg", "Artwork.png",
	"thumb.jpg", "thumb.png",
	"обложка.jpg", "обложка.jpeg", "обложка.png", "обложка.webp",
	"Обложка.jpg", "Обложка.jpeg", "Обложка.png", "Обложка.webp",
}

var coverPrefixes = []string{"cover", "folder", "front", "album", "artwork", "thumb", "обложка"}
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true}

func isImage(name string) bool {
	return imageExts[strings.ToLower(filepath.Ext(name))]
}

// findCoverInDir ищет обложку в директории: приоритетные имена → префиксы → рекурсия.
func findCoverInDir(dir string) (string, error) {
	for _, name := range coverNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	// по префиксу в корне (напр. cover-max.jpg, Cover_600.png)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !isImage(e.Name()) {
				continue
			}
			low := strings.ToLower(e.Name())
			for _, pre := range coverPrefixes {
				if strings.HasPrefix(low, pre) {
					return filepath.Join(dir, e.Name()), nil
				}
			}
		}
	}
	// рекурсивно (Scans/, Artwork/, CD1/ и т.п.), предпочитая крупные файлы
	var best string
	var bestSize int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isImage(d.Name()) {
			return nil
		}
		low := strings.ToLower(d.Name())
		for _, pre := range coverPrefixes {
			if strings.HasPrefix(low, pre) {
				if info, e := d.Info(); e == nil && info.Size() > bestSize {
					best, bestSize = p, info.Size()
				}
			}
		}
		return nil
	})
	if best != "" {
		return best, nil
	}
	// fallback: любое изображение в корне альбома (часто "Artist - Album.jpg").
	// Берём самый крупный файл, чтобы не схватить мелкий логотип.
	if entries, err := os.ReadDir(dir); err == nil {
		var anyImage string
		var anySize int64
		for _, e := range entries {
			if e.IsDir() || !isImage(e.Name()) {
				continue
			}
			if info, err := e.Info(); err == nil && info.Size() > anySize {
				anyImage, anySize = filepath.Join(dir, e.Name()), info.Size()
			}
		}
		if anyImage != "" {
			return anyImage, nil
		}
	}
	// Последний fallback: изображение во вложенной папке (Covers/, Scans/),
	// но не задник/буклет/инлей — иначе на обложке альбома будет Back.png.
	var nested string
	var nestedSize int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isImage(d.Name()) {
			return nil
		}
		low := strings.ToLower(d.Name())
		for _, bad := range []string{"back", "booklet", "inlay", "matrix", "tray", "spine", "obi", "disc", "cd"} {
			if strings.Contains(low, bad) {
				return nil
			}
		}
		if info, e := d.Info(); e == nil && info.Size() > nestedSize {
			nested, nestedSize = p, info.Size()
		}
		return nil
	})
	if nested != "" {
		return nested, nil
	}
	return "", os.ErrNotExist
}

// albumDirFor ищет директорию альбома (полный путь) через индекс артистов,
// затем через MPD find/search.
func albumDirFor(artist, album string) string {
	base := platform.GetEnv("MUSIC_DIR", "/mnt/nas/root/FuzzyHD/Music")
	// 1) индекс артистов (уже собранный) — самый надёжный источник
	if _, byName := getArtistIndex(); byName != nil {
		for a, tracks := range byName {
			if artist != "" && !strings.EqualFold(a, artist) {
				continue
			}
			for _, t := range tracks {
				alb, _ := t["album"].(string)
				if strings.EqualFold(alb, album) {
					f, _ := t["file"].(string)
					return filepath.Join(base, filepath.Dir(f))
				}
			}
		}
	}
	// 2) MPD find
	if m, err := getMPD(); err == nil {
		tryFind := func(args ...string) string {
			lines, err := m.raw("find", args...)
			if err != nil {
				return ""
			}
			for _, l := range lines {
				if strings.HasPrefix(l, "file: ") {
					f := strings.TrimPrefix(l, "file: ")
					return filepath.Join(base, filepath.Dir(f))
				}
			}
			return ""
		}
		if artist != "" {
			if d := tryFind("Album", album, "AlbumArtist", artist); d != "" {
				return d
			}
			if d := tryFind("Album", album, "Artist", artist); d != "" {
				return d
			}
		}
		if d := tryFind("Album", album); d != "" {
			return d
		}
	}
	return ""
}

// albumDirsFor — все каталоги изданий альбома (одного артиста).
// Нужно, чтобы найти обложку в другом издании, если в первом её нет.
func albumDirsFor(artist, album string) []string {
	base := platform.GetEnv("MUSIC_DIR", "/mnt/nas/root/FuzzyHD/Music")
	seen := map[string]bool{}
	out := []string{}
	if _, byName := getArtistIndex(); byName != nil {
		for a, tracks := range byName {
			if artist != "" && !strings.EqualFold(a, artist) {
				continue
			}
			for _, t := range tracks {
				alb, _ := t["album"].(string)
				if !strings.EqualFold(alb, album) {
					continue
				}
				f, _ := t["file"].(string)
				d := filepath.Join(base, filepath.Dir(f))
				if !seen[d] {
					seen[d] = true
					out = append(out, d)
				}
			}
		}
	}
	if len(out) == 0 {
		if d := albumDirFor(artist, album); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// extractEmbeddedCover извлекает встроенную обложку через ffmpeg во временный файл.
func extractEmbeddedCover(audioPath string) (string, error) {
	if _, err := os.Stat(audioPath); err != nil {
		return "", err
	}
	h := sha1.Sum([]byte(audioPath))
	out := filepath.Join(os.TempDir(), "nas_cover_"+hex.EncodeToString(h[:8])+".jpg")
	if _, err := os.Stat(out); err == nil {
		return out, nil // кэш
	}
	// Сначала metaflac (точнее для FLAC), затем ffmpeg.
	if _, err := exec.LookPath("metaflac"); err == nil {
		if err := exec.Command("metaflac", "--export-picture-to="+out, audioPath).Run(); err == nil {
			if _, e := os.Stat(out); e == nil {
				return out, nil
			}
		}
	}
	cmd := exec.Command("ffmpeg", "-y", "-i", audioPath, "-an", "-c:v", "copy", "-frames:v", "1", out)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if _, err := os.Stat(out); err != nil {
		return "", os.ErrNotExist
	}
	return out, nil
}

// coverFor — полный поиск обложки: файл в папке → встроенная (ffmpeg).
func coverFor(artist, album string) string {
	dirs := albumDirsFor(artist, album)
	for _, dir := range dirs {
		if p, err := findCoverInDir(dir); err == nil {
			return p
		}
	}
	// встроенная обложка из первого аудиофайла любого издания
	for _, dir := range dirs {
		var firstAudio string
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || firstAudio != "" {
				return nil
			}
			if audioExtsSmart[strings.ToLower(filepath.Ext(d.Name()))] {
				firstAudio = p
			}
			return nil
		})
		if firstAudio != "" {
			if p, err := extractEmbeddedCover(firstAudio); err == nil {
				return p
			}
		}
	}
	return ""
}

// ---------- MusicBrainz / Cover Art Archive ----------

var (
	reBrackets = regexp.MustCompile(`\s*[\(\[\{][^\)\]\}]*[\)\]\}]`)
	reYear     = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	reTagsMB   = regexp.MustCompile(`(?i)\b(cd\d*|disc\d*|remaster(ed)?|deluxe|anniversary|expanded|bonus|live|ep|single)\b`)
	reSpaces   = regexp.MustCompile(`\s+`)
)

// normalizeForSearch убирает мусор из тегов: скобки, годы, CD/Remaster/Deluxe и т.п.
func normalizeForSearch(s string) string {
	s = reBrackets.ReplaceAllString(s, "")
	s = reYear.ReplaceAllString(s, "")
	s = reTagsMB.ReplaceAllString(s, "")
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

type mbRelease struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Date         string `json:"date"`
	ArtistCredit []struct {
		Name string `json:"name"`
	} `json:"artist-credit"`
}

// fetchMusicBrainzRelease ищет релиз в MusicBrainz (с кэшем в Redis на 30 дней).
func fetchMusicBrainzRelease(artist, album string) (*mbRelease, error) {
	cleanArtist := normalizeForSearch(artist)
	cleanAlbum := normalizeForSearch(album)
	if cleanAlbum == "" {
		return nil, fmt.Errorf("empty album")
	}
	key := "mb:release:" + strings.ToLower(cleanArtist) + ":" + strings.ToLower(cleanAlbum)
	if rdb != nil {
		if b, err := rdb.Get(ctx, key).Bytes(); err == nil {
			var r mbRelease
			if json.Unmarshal(b, &r) == nil && r.ID != "" {
				return &r, nil
			}
		}
	}
	q := fmt.Sprintf(`artist:"%s" AND release:"%s"`, cleanArtist, cleanAlbum)
	mbURL := "https://musicbrainz.org/ws/2/release/?query=" + url.QueryEscape(q) + "&fmt=json&limit=1"
	req, _ := http.NewRequest("GET", mbURL, nil)
	req.Header.Set("User-Agent", "nas-stack/1.0.0 (https://github.com/FuzzyHD1993/nas-stack)")
	hc := &http.Client{Timeout: 8 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("musicbrainz HTTP %d", resp.StatusCode)
	}
	var out struct {
		Releases []mbRelease `json:"releases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Releases) == 0 || out.Releases[0].ID == "" {
		return nil, fmt.Errorf("not found")
	}
	r := &out.Releases[0]
	if rdb != nil {
		if b, err := json.Marshal(r); err == nil {
			_ = rdb.Set(ctx, key, b, 30*24*time.Hour).Err()
		}
	}
	return r, nil
}

// musicBrainzCoverURL возвращает прямой URL обложки из Cover Art Archive.
func musicBrainzCoverURL(artist, album string) string {
	rel, err := fetchMusicBrainzRelease(artist, album)
	if err != nil || rel == nil {
		return ""
	}
	return "https://coverartarchive.org/release/" + rel.ID + "/front-500"
}

// ---------- iTunes Search API (резерв/основной при недоступном MusicBrainz) ----------

type itunesAlbum struct {
	CollectionName string `json:"collectionName"`
	ArtistName     string `json:"artistName"`
	ReleaseDate    string `json:"releaseDate"`
	ArtworkURL100  string `json:"artworkUrl100"`
}

func fetchITunesAlbum(artist, album string) (*itunesAlbum, error) {
	term := strings.TrimSpace(normalizeForSearch(artist) + " " + normalizeForSearch(album))
	if term == "" {
		return nil, fmt.Errorf("empty query")
	}
	key := "itunes:album:" + strings.ToLower(term)
	if rdb != nil {
		if b, err := rdb.Get(ctx, key).Bytes(); err == nil {
			var r itunesAlbum
			if json.Unmarshal(b, &r) == nil && r.ArtworkURL100 != "" {
				return &r, nil
			}
		}
	}
	u := "https://itunes.apple.com/search?term=" + url.QueryEscape(term) + "&entity=album&limit=1"
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "nas-stack/1.0.0")
	hc := &http.Client{Timeout: 8 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("itunes HTTP %d", resp.StatusCode)
	}
	var out struct {
		Results []itunesAlbum `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Results) == 0 || out.Results[0].ArtworkURL100 == "" {
		return nil, fmt.Errorf("not found")
	}
	r := &out.Results[0]
	if rdb != nil {
		if b, err := json.Marshal(r); err == nil {
			_ = rdb.Set(ctx, key, b, 30*24*time.Hour).Err()
		}
	}
	return r, nil
}

// itunesCoverURL возвращает обложку 600x600 (iTunes отдаёт 100x100bb).
func itunesCoverURL(artist, album string) string {
	r, err := fetchITunesAlbum(artist, album)
	if err != nil || r == nil {
		return ""
	}
	u := strings.ReplaceAll(r.ArtworkURL100, "100x100bb", "600x600bb")
	return strings.ReplaceAll(u, "100x100-75", "600x600-75")
}
