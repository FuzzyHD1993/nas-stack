package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
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

const maxCoverBytes = 25 << 20

// reBadCover — задники/буклеты/инлеи не должны становиться обложкой.
// Границы по \p{L}, чтобы не резать "sacd", "discovery", "playback".
var reBadCover = regexp.MustCompile(`(?i)(^|[^\p{L}])(back|backcover|booklet|inlay|matrix|tray|spine|obi|disc[0-9]*|cd[0-9]+)([^\p{L}]|$)`)

func isBadCoverName(name string) bool {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	return reBadCover.MatchString(base)
}

// imageContentType определяет тип по содержимому, а не по расширению:
// ffmpeg с "-c:v copy" может положить PNG/WebP в файл .jpg.
func imageContentType(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	switch ct := http.DetectContentType(head[:n]); ct {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return ct
	}
	return ""
}

// validCoverFile — обычный непустой файл разумного размера с картинкой внутри.
func validCoverFile(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 || fi.Size() > maxCoverBytes {
		return false
	}
	return imageContentType(p) != ""
}

// findCoverInDir ищет обложку в директории: приоритетные имена → префиксы → рекурсия.
// Результат обязан быть обычным файлом внутри каталога альбома: симлинк
// cover.jpg -> /etc/shadow отвергается.
func findCoverInDir(dir string) (string, error) {
	p, err := findCoverInDirRaw(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil || !withinRoot(dir, real) {
		return "", os.ErrNotExist
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.Mode().IsRegular() {
		return "", os.ErrNotExist
	}
	return real, nil
}

func findCoverInDirRaw(dir string) (string, error) {
	// 1) точные имена в порядке приоритета. Stat: каталог с именем cover.jpg
	// не должен блокировать поиск настоящей обложки, а симлинк на файл — работать.
	for _, name := range coverNames {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", os.ErrNotExist
	}

	// 2) префиксы: порядок coverPrefixes важнее алфавитного порядка файлов.
	for _, pre := range coverPrefixes {
		for _, e := range entries {
			if !e.Type().IsRegular() || !isImage(e.Name()) || isBadCoverName(e.Name()) {
				continue
			}
			if strings.HasPrefix(strings.ToLower(e.Name()), pre) {
				return filepath.Join(dir, e.Name()), nil
			}
		}
	}

	// 3) рекурсивно (Scans/, Artwork/, CD1/ и т.п.), предпочитая крупные файлы
	var best string
	var bestSize int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() ||
			!isImage(d.Name()) || isBadCoverName(d.Name()) {
			return nil
		}
		low := strings.ToLower(d.Name())
		for _, pre := range coverPrefixes {
			if strings.HasPrefix(low, pre) {
				if info, e := d.Info(); e == nil && info.Size() > bestSize {
					best, bestSize = p, info.Size()
				}
				break
			}
		}
		return nil
	})
	if best != "" {
		return best, nil
	}

	// 4) fallback: любое изображение в корне альбома (часто "Artist - Album.jpg").
	// Берём самый крупный файл, чтобы не схватить мелкий логотип.
	var anyImage string
	var anySize int64
	for _, e := range entries {
		if !e.Type().IsRegular() || !isImage(e.Name()) || isBadCoverName(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil && info.Size() > anySize {
			anyImage, anySize = filepath.Join(dir, e.Name()), info.Size()
		}
	}
	if anyImage != "" {
		return anyImage, nil
	}

	// 5) Последний fallback: изображение во вложенной папке (Covers/, Scans/),
	// но не задник/буклет/инлей — иначе на обложке альбома будет Back.png.
	var nested string
	var nestedSize int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() ||
			!isImage(d.Name()) || isBadCoverName(d.Name()) {
			return nil
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
	// 1) индекс артистов (уже собранный) — самый надёжный источник
	if _, byName, _ := getArtistIndex(); byName != nil {
		for a, tracks := range byName {
			if artist != "" && !strings.EqualFold(a, artist) {
				continue
			}
			for _, t := range tracks {
				if strings.EqualFold(t.Album, album) {
					if d := safeAlbumDir(t.File); d != "" {
						return d
					}
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
					if d := safeAlbumDir(strings.TrimPrefix(l, "file: ")); d != "" {
						return d
					}
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
	seen := map[string]bool{}
	out := []string{}
	if _, byName, _ := getArtistIndex(); byName != nil {
		for a, tracks := range byName {
			if artist != "" && !strings.EqualFold(a, artist) {
				continue
			}
			for _, t := range tracks {
				if !strings.EqualFold(t.Album, album) {
					continue
				}
				d := safeAlbumDir(t.File)
				if d == "" || seen[d] {
					continue
				}
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	if len(out) == 0 {
		if d := albumDirFor(artist, album); d != "" {
			out = append(out, d)
		}
	}
	// Детерминированный порядок: map обходится случайно, иначе обложка
	// одного и того же альбома «мигает» между изданиями.
	sort.Strings(out)
	return out
}

// extractEmbeddedCover извлекает встроенную обложку через ffmpeg во временный файл.
func extractEmbeddedCover(audioPath string) (string, error) {
	fi, err := os.Stat(audioPath)
	if err != nil || !fi.Mode().IsRegular() {
		return "", os.ErrNotExist
	}
	dir, err := mediaCacheDir()
	if err != nil {
		return "", err
	}
	// Ключ с mtime+size: после перезаписи тегов старая обложка не отдаётся.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d",
		audioPath, fi.Size(), fi.ModTime().UnixNano())))
	out := filepath.Join(dir, "cover-"+hex.EncodeToString(sum[:16])+".jpg")
	if validCoverFile(out) {
		return out, nil
	}

	// Один ffmpeg/metaflac на ключ: параллельные запросы не пишут один файл.
	mu := cacheLockFor(out)
	mu.Lock()
	defer mu.Unlock()
	if validCoverFile(out) {
		return out, nil
	}

	tmp, err := os.CreateTemp(dir, ".cover-*.jpg")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpName)

	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// metaflac отказывается перезаписывать существующий файл.
	if err := os.Remove(tmpName); err != nil && !os.IsNotExist(err) {
		return "", err
	}

	// Сначала metaflac (точнее для FLAC), затем ffmpeg.
	if _, err := exec.LookPath("metaflac"); err == nil {
		cmd := exec.CommandContext(cctx, "metaflac", "--export-picture-to="+tmpName, audioPath)
		if err := cmd.Run(); err == nil && validCoverFile(tmpName) {
			if err := os.Rename(tmpName, out); err != nil {
				return "", err
			}
			return out, nil
		}
	}
	cmd := exec.CommandContext(cctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-protocol_whitelist", "file",
		"-i", "file:"+audioPath,
		"-an", "-c:v", "copy", "-frames:v", "1", "-f", "image2", tmpName)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if !validCoverFile(tmpName) {
		return "", os.ErrNotExist
	}
	if err := os.Rename(tmpName, out); err != nil {
		return "", err
	}
	return out, nil
}

type coverMemoEntry struct {
	path string
	exp  time.Time
}

const coverMemoTTL = 5 * time.Minute

var (
	coverGroup  singleflight.Group
	coverMemoMu sync.RWMutex
	coverMemo   = map[string]coverMemoEntry{}
)

// coverFor — мемоизированный поиск (включая отрицательный результат) и
// схлопывание параллельных запросов: сетка альбомов не должна запускать
// N×ffmpeg и N×MusicBrainz.
func coverFor(artist, album string) string {
	key := strings.ToLower(strings.TrimSpace(artist)) + "\x00" + strings.ToLower(strings.TrimSpace(album))

	coverMemoMu.RLock()
	e, ok := coverMemo[key]
	coverMemoMu.RUnlock()
	if ok && time.Now().Before(e.exp) {
		return e.path
	}

	v, _, _ := coverGroup.Do(key, func() (any, error) {
		coverMemoMu.RLock()
		e, ok := coverMemo[key]
		coverMemoMu.RUnlock()
		if ok && time.Now().Before(e.exp) {
			return e.path, nil
		}
		p := coverForUncached(artist, album)
		coverMemoMu.Lock()
		if len(coverMemo) > 4096 {
			coverMemo = map[string]coverMemoEntry{}
		}
		coverMemo[key] = coverMemoEntry{path: p, exp: time.Now().Add(coverMemoTTL)}
		coverMemoMu.Unlock()
		return p, nil
	})
	p, _ := v.(string)
	return p
}

func coverForUncached(artist, album string) string {
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
			if d.Type().IsRegular() && audioExts[strings.ToLower(filepath.Ext(d.Name()))] {
				firstAudio = p
				return filepath.SkipAll
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

// ---------- общие помощники для внешних API ----------

const (
	mbUserAgent     = "nas-stack/1.0.0 (https://github.com/FuzzyHD1993/nas-stack)"
	maxUpstreamJSON = 2 << 20 // 2 MiB
)

// coverHTTP — общий клиент: таймаут и запрет небезопасных редиректов.
var coverHTTP = &http.Client{
	Timeout: 8 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("insecure redirect")
		}
		return nil
	},
}

// getJSON — единственная точка внешних JSON-запросов: лимит тела,
// проверка Content-Type и дочитывание тела для переиспользования соединения.
func getJSON(rawURL, ua string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := coverHTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return fmt.Errorf("upstream content-type %q", ct)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamJSON)).Decode(dst)
}

// coverCacheKey — однозначный ключ: NUL-разделитель исключает коллизии
// ("a:b"+"c" vs "a"+"b:c").
func coverCacheKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(p))))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

var luceneEscaper = strings.NewReplacer(
	`\`, `\\`, `"`, `\"`, `(`, `\(`, `)`, `\)`, `:`, `\:`, `[`, `\[`, `]`, `\]`,
	`{`, `\{`, `}`, `\}`, `+`, `\+`, `-`, `\-`, `!`, `\!`, `^`, `\^`, `~`, `\~`,
	`*`, `\*`, `?`, `\?`, `/`, `\/`,
)

// escapeLucene экранирует метасимволы Lucene: кавычка в названии альбома
// больше не ломает (и не подменяет) запрос к MusicBrainz.
func escapeLucene(s string) string { return luceneEscaper.Replace(s) }

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func coverArtArchiveURL(id string) string {
	if !uuidRe.MatchString(id) {
		return ""
	}
	return "https://coverartarchive.org/release/" + url.PathEscape(id) + "/front-500"
}

// safeArtworkURL — allowlist схемы/хоста для URL из внешнего API/кэша:
// значение уходит в Location, поэтому без проверки это open redirect.
func safeArtworkURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "coverartarchive.org" || host == "itunes.apple.com" ||
		strings.HasSuffix(host, ".mzstatic.com") {
		return u.String()
	}
	return ""
}

var artworkSize = strings.NewReplacer(
	"100x100bb", "600x600bb",
	"100x100-75", "600x600-75",
)

func itunesArtworkURL(raw string) string {
	return safeArtworkURL(artworkSize.Replace(raw))
}

// ---------- MusicBrainz / Cover Art Archive ----------

var (
	reBrackets = regexp.MustCompile(`\s*[\(\[\{][^\)\]\}]*[\)\]\}]`)
	reYear     = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	reTagsMB   = regexp.MustCompile(`(?i)\b(cd\d*|disc\d*|remaster(ed)?|edition|deluxe|anniversary|expanded|bonus|reissue)\b`)
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
	key := "mb:release:v2:" + coverCacheKey(cleanArtist, cleanAlbum)
	if rdb != nil {
		if b, err := rdb.Get(ctx, key).Bytes(); err == nil {
			var r mbRelease
			if json.Unmarshal(b, &r) == nil && uuidRe.MatchString(r.ID) {
				return &r, nil
			}
		}
	}
	q := fmt.Sprintf(`artist:"%s" AND release:"%s"`,
		escapeLucene(cleanArtist), escapeLucene(cleanAlbum))
	mbURL := "https://musicbrainz.org/ws/2/release/?query=" + url.QueryEscape(q) + "&fmt=json&limit=1"
	var out struct {
		Releases []mbRelease `json:"releases"`
	}
	if err := getJSON(mbURL, mbUserAgent, &out); err != nil {
		return nil, err
	}
	if len(out.Releases) == 0 || !uuidRe.MatchString(out.Releases[0].ID) {
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
	return coverArtArchiveURL(rel.ID)
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
	if term == "" || len(term) > 300 {
		return nil, fmt.Errorf("empty query")
	}
	key := "itunes:album:v2:" + coverCacheKey(term)
	if rdb != nil {
		if b, err := rdb.Get(ctx, key).Bytes(); err == nil {
			var r itunesAlbum
			if json.Unmarshal(b, &r) == nil && safeArtworkURL(r.ArtworkURL100) != "" {
				return &r, nil
			}
		}
	}
	u := "https://itunes.apple.com/search?term=" + url.QueryEscape(term) + "&entity=album&limit=1"
	var out struct {
		Results []itunesAlbum `json:"results"`
	}
	if err := getJSON(u, "nas-stack/1.0.0", &out); err != nil {
		return nil, err
	}
	if len(out.Results) == 0 || safeArtworkURL(out.Results[0].ArtworkURL100) == "" {
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
	return itunesArtworkURL(r.ArtworkURL100)
}
