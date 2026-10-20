package main

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	artistCacheTTL    = 10 * time.Minute
	artistNegativeTTL = 30 * time.Second
	artistScanTimeout = 45 * time.Second
	unknownArtist     = "Неизвестный"
	noAlbumTitle      = "Без альбома"
	maxArtistNameLen  = 512
)

type artistTrack struct {
	File     string `json:"file"`
	Title    string `json:"title"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
}

type artistSummary struct {
	Name       string `json:"name"`
	TrackCount int    `json:"track_count"`
	AlbumCount int    `json:"album_count"`
}

// artistLib — кэш индекса артистов из базы MPD.
// mu защищает снимок (artists/byName/expires/retryAt), buildMu — singleflight
// на перестройку: параллельные запросы не запускают N сканов listallinfo.
type artistLib struct {
	mu      sync.RWMutex
	artists []artistSummary
	byName  map[string][]artistTrack
	expires time.Time
	retryAt time.Time // negative cache: не долбим MPD при ошибке
	lastErr error     // причина последней неудачной сборки (для 503, а не 404)
	buildMu sync.Mutex
}

var errArtistIndexUnavailable = errors.New("artist index unavailable")

var (
	artistCache artistLib
	queueMu     sync.Mutex // сериализует clear→add*→play как одну транзакцию
)

// normArtist нормализует ключ: "Muse " и "  Muse" — один артист.
func normArtist(s string) string { return strings.Join(strings.Fields(s), " ") }

// buildArtistIndex собирает индекс артистов из MPD (listallinfo) одним проходом.
func buildArtistIndex() error {
	c, err := getMPD()
	if err != nil {
		return artistIndexFailed(err)
	}
	lines, err := c.rawTimeout(artistScanTimeout, "listallinfo")
	if err != nil {
		return artistIndexFailed(err)
	}

	type rawTrack struct {
		file, artist, albumArtist, album, title string
		duration                                int
	}
	tracks := make([]rawTrack, 0, len(lines)/8+1)
	var cur *rawTrack
	flush := func() {
		if cur == nil || cur.file == "" {
			cur = nil
			return
		}
		// AlbumArtist приоритетнее Artist независимо от порядка тегов в ответе MPD.
		if cur.albumArtist != "" {
			cur.artist = cur.albumArtist
		}
		if normArtist(cur.artist) == "" {
			cur.artist = unknownArtist
		}
		tracks = append(tracks, *cur)
		cur = nil
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "file: ") {
			flush()
			cur = &rawTrack{file: strings.TrimPrefix(l, "file: ")}
			continue
		}
		if cur == nil {
			continue
		}
		i := strings.Index(l, ": ")
		if i < 0 {
			continue
		}
		switch strings.ToLower(l[:i]) {
		case "artist":
			cur.artist = l[i+2:]
		case "albumartist":
			cur.albumArtist = l[i+2:]
		case "album":
			cur.album = l[i+2:]
		case "title":
			cur.title = l[i+2:]
		case "time", "duration":
			cur.duration = int(parseFloat(l[i+2:]))
		}
	}
	flush()

	counts := make(map[string]int, len(tracks))
	albumSets := make(map[string]map[string]struct{}, len(tracks))
	byName := make(map[string][]artistTrack, len(tracks))
	for _, t := range tracks {
		name := normArtist(t.artist)
		if name == "" {
			name = unknownArtist
		}
		album := normArtist(t.album)
		if album == "" {
			album = noAlbumTitle // считаем ровно так же, как /artist/:name
		}
		counts[name]++
		if albumSets[name] == nil {
			albumSets[name] = map[string]struct{}{}
		}
		albumSets[name][album] = struct{}{}
		byName[name] = append(byName[name], artistTrack{
			File: t.file, Title: t.title, Album: t.album, Duration: t.duration,
		})
	}

	artists := make([]artistSummary, 0, len(counts))
	for name, cnt := range counts {
		artists = append(artists, artistSummary{
			Name: name, TrackCount: cnt, AlbumCount: len(albumSets[name]),
		})
	}
	sort.SliceStable(artists, func(i, j int) bool {
		li, lj := strings.ToLower(artists[i].Name), strings.ToLower(artists[j].Name)
		if li != lj {
			return li < lj
		}
		return artists[i].Name < artists[j].Name // детерминированный tie-break
	})

	artistCache.mu.Lock()
	artistCache.artists = artists
	artistCache.byName = byName
	artistCache.expires = time.Now().Add(artistCacheTTL)
	artistCache.retryAt = time.Time{}
	artistCache.lastErr = nil
	artistCache.mu.Unlock()
	return nil
}

func artistIndexFailed(err error) error {
	artistCache.mu.Lock()
	artistCache.retryAt = time.Now().Add(artistNegativeTTL)
	artistCache.lastErr = err
	artistCache.mu.Unlock()
	log.Printf("artists: index build failed: %v", err)
	return err
}

// getArtistIndex отдаёт свежий снимок, при ошибке сборки — последний удачный
// (stale-while-revalidate), не запуская при этом больше одной сборки.
// Если данных нет вовсе, возвращает ошибку, чтобы хендлеры отдали 503, а не
// пустой успешный список.
func getArtistIndex() ([]artistSummary, map[string][]artistTrack, error) {
	now := time.Now()

	artistCache.mu.RLock()
	if now.Before(artistCache.expires) && artistCache.artists != nil {
		a, b := artistCache.artists, artistCache.byName
		artistCache.mu.RUnlock()
		return a, b, nil
	}
	inBackoff := now.Before(artistCache.retryAt)
	artistCache.mu.RUnlock()

	if !inBackoff {
		artistCache.buildMu.Lock()
		artistCache.mu.RLock()
		fresh := time.Now().Before(artistCache.expires) && artistCache.artists != nil
		wait := time.Now().Before(artistCache.retryAt)
		artistCache.mu.RUnlock()
		if !fresh && !wait {
			_ = buildArtistIndex()
		}
		artistCache.buildMu.Unlock()
	}

	artistCache.mu.RLock()
	defer artistCache.mu.RUnlock()
	if artistCache.artists != nil {
		return artistCache.artists, artistCache.byName, nil // stale-while-revalidate
	}
	err := artistCache.lastErr
	if err == nil {
		err = errArtistIndexUnavailable
	}
	return []artistSummary{}, map[string][]artistTrack{}, err
}

// lookupArtist ищет точное совпадение, затем — детерминированный
// case-insensitive фолбэк (минимальный ключ, а не случайный из итератора map).
func lookupArtist(byName map[string][]artistTrack, name string) ([]artistTrack, string, bool) {
	if tracks, ok := byName[name]; ok {
		return tracks, name, true
	}
	bestKey := ""
	for k := range byName {
		if strings.EqualFold(k, name) && (bestKey == "" || k < bestKey) {
			bestKey = k
		}
	}
	if bestKey == "" {
		return nil, name, false
	}
	return byName[bestKey], bestKey, true
}

// groupAlbums группирует треки по альбомам ровно так же, как считает
// album_count в buildArtistIndex: пустой тег попадает в noAlbumTitle.
func groupAlbums(tracks []artistTrack) map[string][]artistTrack {
	albums := make(map[string][]artistTrack)
	for _, t := range tracks {
		alb := normArtist(t.Album)
		if alb == "" {
			alb = noAlbumTitle
		}
		albums[alb] = append(albums[alb], t)
	}
	return albums
}

// artistNameParam декодирует сегмент пути. url.QueryUnescape здесь неверен:
// он превращает '+' в пробел. Ошибка декодирования — 400, а не тихий 404.
func artistNameParam(c fiber.Ctx) (string, error) {
	name, err := url.PathUnescape(c.Params("name"))
	if err != nil {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid artist name")
	}
	name = normArtist(name)
	if name == "" || len(name) > maxArtistNameLen {
		return "", fiber.NewError(fiber.StatusBadRequest, "invalid artist name")
	}
	return name, nil
}

// replaceQueueAndPlay выполняет clear→add*→play как одну транзакцию:
// mpdClient.mu защищает только одну команду, поэтому без него очереди двух
// запросов перемешиваются.
func replaceQueueAndPlay(m *mpdClient, files []string) (int, error) {
	queueMu.Lock()
	defer queueMu.Unlock()
	return replaceQueueAndPlayLocked(m, files)
}

// replaceQueueAndPlayLocked требует уже захваченного queueMu.
// clear не выполняется, пока не подтверждено, что есть что добавлять.
func replaceQueueAndPlayLocked(m *mpdClient, files []string) (int, error) {
	rels := make([]string, 0, len(files))
	for _, f := range files {
		rel, ok := relMusicPath(f)
		if !ok {
			log.Printf("mpd add: пропуск %q (вне MUSIC_DIR)", f)
			continue
		}
		rels = append(rels, rel)
	}
	if len(rels) == 0 {
		return 0, fmt.Errorf("no tracks to play")
	}
	if err := m.exec("clear"); err != nil {
		return 0, fmt.Errorf("clear: %w", err)
	}
	added := 0
	for _, f := range rels {
		if err := m.exec("add", f); err != nil {
			log.Printf("mpd add %q: %v", f, err)
			continue
		}
		added++
	}
	if added == 0 {
		return 0, fmt.Errorf("no tracks added (stale index?)")
	}
	if err := m.exec("play"); err != nil {
		return added, fmt.Errorf("play: %w", err)
	}
	return added, nil
}

func artistsRoutes(app *fiber.App) {
	// GET /artists — список артистов с количеством треков/альбомов.
	app.Get("/artists", func(c fiber.Ctx) error {
		artists, _, err := getArtistIndex()
		if err != nil {
			log.Printf("artists: %v", err)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "artist index unavailable", "artists": []artistSummary{}, "count": 0,
			})
		}
		if artists == nil {
			artists = []artistSummary{}
		}
		return c.JSON(fiber.Map{"artists": artists, "count": len(artists)})
	})

	// GET /artist/:name — все треки артиста, сгруппированные по альбомам.
	app.Get("/artist/:name", func(c fiber.Ctx) error {
		name, err := artistNameParam(c)
		if err != nil {
			return err
		}
		_, byName, err := getArtistIndex()
		if err != nil {
			log.Printf("artist %q: %v", name, err)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artist index unavailable"})
		}
		tracks, canonical, ok := lookupArtist(byName, name)
		if !ok {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": "artist not found", "artist": name, "tracks": []artistTrack{},
			})
		}
		albums := groupAlbums(tracks)
		return c.JSON(fiber.Map{
			"artist": canonical, "tracks": tracks, "albums": albums,
			"track_count": len(tracks), "album_count": len(albums),
		})
	})

	// POST /play/artist/:name — очистить очередь, добавить все треки артиста и играть.
	app.Post("/play/artist/:name", func(c fiber.Ctx) error {
		name, err := artistNameParam(c)
		if err != nil {
			return err
		}
		_, byName, err := getArtistIndex()
		if err != nil {
			log.Printf("play artist %q: %v", name, err)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artist index unavailable"})
		}
		tracks, _, ok := lookupArtist(byName, name)
		if !ok || len(tracks) == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "artist not found"})
		}
		m, err := getMPD()
		if err != nil {
			log.Printf("play artist: mpd: %v", err)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		files := make([]string, 0, len(tracks))
		for _, t := range tracks {
			files = append(files, t.File)
		}
		added, err := replaceQueueAndPlay(m, files)
		if err != nil {
			log.Printf("play artist %q: %v", name, err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "queue update failed", "added": added})
		}
		return c.JSON(fiber.Map{"status": "ok", "count": added})
	})
}
