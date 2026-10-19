package main

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
)

// artistLib — кэш индекса артистов из базы MPD.
type artistLib struct {
	sync.Mutex
	artists []fiber.Map            // [{name, track_count, album_count}]
	byName  map[string][]fiber.Map // artist → треки [{file,title,album,duration}]
	expires time.Time
}

var artistCache artistLib

// buildArtistIndex собирает индекс артистов из MPD (listallinfo) одним проходом.
func buildArtistIndex() {
	c, err := getMPD()
	if err != nil {
		return
	}
	lines, err := c.raw("listallinfo")
	if err != nil {
		return
	}
	type track struct {
		file, artist, album, title string
		duration                   int
	}
	tracks := []track{}
	var cur *track
	flush := func() {
		if cur != nil && cur.file != "" {
			// AlbumArtist предпочтительнее, но используем то, что есть.
			if cur.artist == "" {
				cur.artist = "Неизвестный"
			}
			tracks = append(tracks, *cur)
		}
		cur = nil
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "file: ") {
			flush()
			cur = &track{file: strings.TrimPrefix(l, "file: ")}
			continue
		}
		if cur == nil {
			continue
		}
		i := strings.Index(l, ": ")
		if i < 0 {
			continue
		}
		k := strings.ToLower(l[:i])
		v := l[i+2:]
		switch k {
		case "artist":
			cur.artist = v
		case "albumartist":
			cur.artist = v
		case "album":
			cur.album = v
		case "title":
			cur.title = v
		case "time", "duration":
			cur.duration = int(parseFloat(v))
		}
	}
	flush()

	counts := map[string]int{}
	albumSets := map[string]map[string]bool{}
	byName := map[string][]fiber.Map{}
	for _, t := range tracks {
		counts[t.artist]++
		if albumSets[t.artist] == nil {
			albumSets[t.artist] = map[string]bool{}
		}
		if t.album != "" {
			albumSets[t.artist][t.album] = true
		}
		byName[t.artist] = append(byName[t.artist], fiber.Map{
			"file": t.file, "title": t.title, "album": t.album, "duration": t.duration,
		})
	}
	artists := []fiber.Map{}
	for name, cnt := range counts {
		artists = append(artists, fiber.Map{
			"name": name, "track_count": cnt, "album_count": len(albumSets[name]),
		})
	}
	sort.Slice(artists, func(i, j int) bool {
		return strings.ToLower(artists[i]["name"].(string)) < strings.ToLower(artists[j]["name"].(string))
	})

	artistCache.Lock()
	artistCache.artists = artists
	artistCache.byName = byName
	artistCache.expires = time.Now().Add(10 * time.Minute)
	artistCache.Unlock()
}

func getArtistIndex() ([]fiber.Map, map[string][]fiber.Map) {
	artistCache.Lock()
	if time.Now().Before(artistCache.expires) && artistCache.artists != nil {
		a, b := artistCache.artists, artistCache.byName
		artistCache.Unlock()
		return a, b
	}
	artistCache.Unlock()
	buildArtistIndex()
	artistCache.Lock()
	defer artistCache.Unlock()
	return artistCache.artists, artistCache.byName
}

func artistsRoutes(app *fiber.App) {
	// GET /artists — список артистов с количеством треков/альбомов.
	app.Get("/artists", func(c fiber.Ctx) error {
		artists, _ := getArtistIndex()
		return c.JSON(fiber.Map{"artists": artists, "count": len(artists)})
	})

	// GET /artist/:name — все треки артиста, сгруппированные по альбомам.
	app.Get("/artist/:name", func(c fiber.Ctx) error {
		name, _ := url.QueryUnescape(c.Params("name"))
		_, byName := getArtistIndex()
		tracks, ok := byName[name]
		if !ok {
			// нечёткий поиск (case-insensitive)
			for k, v := range byName {
				if strings.EqualFold(k, name) {
					tracks, ok = v, true
					name = k
					break
				}
			}
		}
		if !ok {
			return c.Status(404).JSON(fiber.Map{"error": "artist not found", "artist": name, "tracks": []fiber.Map{}})
		}
		albums := map[string][]fiber.Map{}
		for _, t := range tracks {
			alb := t["album"].(string)
			if alb == "" {
				alb = "Без альбома"
			}
			albums[alb] = append(albums[alb], t)
		}
		return c.JSON(fiber.Map{
			"artist": name, "tracks": tracks, "albums": albums,
			"track_count": len(tracks), "album_count": len(albums),
		})
	})

	// POST /play/artist/:name — очистить очередь, добавить все треки артиста и играть.
	app.Post("/play/artist/:name", func(c fiber.Ctx) error {
		name, _ := url.QueryUnescape(c.Params("name"))
		_, byName := getArtistIndex()
		tracks, ok := byName[name]
		if !ok {
			for k, v := range byName {
				if strings.EqualFold(k, name) {
					tracks = v
					ok = true
					break
				}
			}
		}
		if !ok || len(tracks) == 0 {
			return c.Status(404).JSON(fiber.Map{"error": "artist not found"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": err.Error()})
		}
		_ = m.exec("clear")
		for _, t := range tracks {
			_ = m.exec("add", t["file"].(string))
		}
		_ = m.exec("play")
		return c.JSON(fiber.Map{"status": "ok", "count": len(tracks)})
	})
}
