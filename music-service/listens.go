package main

import (
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
)

// migrateListens создаёт таблицу истории прослушиваний.
func migrateListens() error {
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS listens (
			id SERIAL PRIMARY KEY,
			track_path TEXT NOT NULL,
			artist TEXT,
			album TEXT,
			title TEXT,
			duration_sec INT,
			played_at TIMESTAMPTZ DEFAULT NOW(),
			completed BOOLEAN DEFAULT FALSE
		);
		CREATE INDEX IF NOT EXISTS idx_listens_track ON listens(track_path);
		CREATE INDEX IF NOT EXISTS idx_listens_played ON listens(played_at DESC);
		CREATE INDEX IF NOT EXISTS idx_listens_artist ON listens(artist);
	`)
	return err
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// ---------- индексация музыкальной библиотеки (с кэшем) ----------

var audioExtsSmart = map[string]bool{
	".flac": true, ".mp3": true, ".ogg": true, ".oga": true, ".opus": true,
	".wav": true, ".m4a": true, ".aac": true, ".dsf": true, ".dff": true,
	".ape": true, ".wv": true,
}

type musicFile struct {
	Path    string
	ModTime time.Time
}

var (
	musicIdxMu   sync.Mutex
	musicIdx     []musicFile
	musicIdxTime time.Time
)

func musicIndex() []musicFile {
	musicIdxMu.Lock()
	defer musicIdxMu.Unlock()
	if time.Since(musicIdxTime) < 10*time.Minute && musicIdx != nil {
		return musicIdx
	}
	base := platform.GetEnv("MUSIC_DIR", "/mnt/nas/root/FuzzyHD/Music")
	files := []musicFile{}
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if audioExtsSmart[strings.ToLower(filepath.Ext(p))] {
			if info, e := d.Info(); e == nil {
				rel, _ := filepath.Rel(base, p)
				files = append(files, musicFile{Path: filepath.ToSlash(rel), ModTime: info.ModTime()})
			}
		}
		return nil
	})
	musicIdx = files
	musicIdxTime = time.Now()
	return files
}

func trackMap(path, artist, album, title string) fiber.Map {
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return fiber.Map{"track_path": path, "artist": artist, "album": album, "title": title}
}

// ---------- умные плейлисты ----------

func smartRecent(limit int) ([]fiber.Map, error) {
	files := musicIndex()
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.After(files[j].ModTime) })
	out := []fiber.Map{}
	for i, f := range files {
		if i >= limit {
			break
		}
		out = append(out, trackMap(f.Path, "", "", ""))
	}
	return out, nil
}

func smartTopPeriod(days, limit int) ([]fiber.Map, error) {
	if db == nil {
		return []fiber.Map{}, nil
	}
	rows, err := db.Query(ctx, `
		SELECT track_path, COALESCE(artist,''), COALESCE(album,''), COALESCE(title,''), COUNT(*) AS cnt
		FROM listens
		WHERE played_at > NOW() - make_interval(days => $1)
		GROUP BY track_path, artist, album, title
		ORDER BY cnt DESC
		LIMIT $2`, days, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []fiber.Map{}
	for rows.Next() {
		var p, a, al, t string
		var cnt int
		if rows.Scan(&p, &a, &al, &t, &cnt) == nil {
			m := trackMap(p, a, al, t)
			m["count"] = cnt
			out = append(out, m)
		}
	}
	return out, nil
}

func smartForgotten(limit int) ([]fiber.Map, error) {
	last := map[string]time.Time{}
	if db != nil {
		if rows, err := db.Query(ctx, `SELECT track_path, MAX(played_at) FROM listens GROUP BY track_path`); err == nil {
			for rows.Next() {
				var p string
				var ts time.Time
				if rows.Scan(&p, &ts) == nil {
					last[p] = ts
				}
			}
			rows.Close()
		}
	}
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	type cand struct {
		f    musicFile
		when time.Time
	}
	cands := []cand{}
	for _, f := range musicIndex() {
		if when, ok := last[f.Path]; ok {
			if when.Before(cutoff) {
				cands = append(cands, cand{f, when})
			}
		} else {
			cands = append(cands, cand{f, time.Time{}})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].when.Before(cands[j].when) })
	out := []fiber.Map{}
	for i, c := range cands {
		if i >= limit {
			break
		}
		out = append(out, trackMap(c.f.Path, "", "", ""))
	}
	return out, nil
}

func smartWave(limit int) ([]fiber.Map, error) {
	if db == nil {
		return []fiber.Map{}, nil
	}
	// Последние 50 прослушиваний → любимые артисты/альбомы.
	artists := map[string]bool{}
	albums := map[string]bool{}
	if rows, err := db.Query(ctx, `
		SELECT COALESCE(artist,''), COALESCE(album,'') FROM listens
		ORDER BY played_at DESC LIMIT 50`); err == nil {
		for rows.Next() {
			var a, al string
			if rows.Scan(&a, &al) == nil {
				if a != "" {
					artists[a] = true
				}
				if al != "" {
					albums[al] = true
				}
			}
		}
		rows.Close()
	}
	// Что слушали за 14 дней (исключаем).
	recent := map[string]bool{}
	if rows, err := db.Query(ctx, `SELECT DISTINCT track_path FROM listens WHERE played_at > NOW() - INTERVAL '14 days'`); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				recent[p] = true
			}
		}
		rows.Close()
	}
	// Кандидаты: треки тех же артистов/альбомов (по пути), не слушанные 14+ дней.
	pool := []fiber.Map{}
	for _, f := range musicIndex() {
		if recent[f.Path] {
			continue
		}
		dir := filepath.Base(filepath.Dir(f.Path))
		match := false
		for a := range artists {
			if strings.Contains(f.Path, a) {
				match = true
				break
			}
		}
		if !match {
			for al := range albums {
				if al != "" && dir == al {
					match = true
					break
				}
			}
		}
		if match {
			pool = append(pool, trackMap(f.Path, "", "", ""))
		}
	}
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if len(pool) > limit {
		pool = pool[:limit]
	}
	return pool, nil
}

// ---------- маршруты ----------

func listensRoutes(app *fiber.App) {
	app.Get("/playlists/smart", func(c fiber.Ctx) error {
		t := c.Query("type", "top_month")
		limit := 50
		if v, err := strconv.Atoi(c.Query("limit", "50")); err == nil && v > 0 && v <= 500 {
			limit = v
		}
		var tracks []fiber.Map
		var err error
		switch t {
		case "recent":
			tracks, err = smartRecent(limit)
		case "top_month":
			tracks, err = smartTopPeriod(30, limit)
		case "top_year":
			tracks, err = smartTopPeriod(365, limit)
		case "forgotten":
			tracks, err = smartForgotten(limit)
		case "wave":
			tracks, err = smartWave(limit)
		default:
			return c.Status(400).JSON(fiber.Map{"error": "unknown type"})
		}
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"type": t, "tracks": tracks, "count": len(tracks)})
	})

	app.Get("/listens", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		limit := 100
		if v, err := strconv.Atoi(c.Query("limit", "100")); err == nil && v > 0 && v <= 1000 {
			limit = v
		}
		rows, err := db.Query(ctx, `
			SELECT track_path, COALESCE(artist,''), COALESCE(album,''), COALESCE(title,''),
			       COALESCE(duration_sec,0), completed, played_at
			FROM listens ORDER BY played_at DESC LIMIT $1`, limit)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		out := []fiber.Map{}
		for rows.Next() {
			var p, a, al, t string
			var dur int
			var completed bool
			var at time.Time
			if rows.Scan(&p, &a, &al, &t, &dur, &completed, &at) == nil {
				out = append(out, fiber.Map{"track_path": p, "artist": a, "album": al, "title": t,
					"duration_sec": dur, "completed": completed, "played_at": at})
			}
		}
		return c.JSON(fiber.Map{"listens": out, "count": len(out)})
	})

	app.Get("/listens/stats/daily", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		days := 30
		if v, err := strconv.Atoi(c.Query("days", "30")); err == nil && v > 0 && v <= 365 {
			days = v
		}
		rows, err := db.Query(ctx, `
			SELECT TO_CHAR(DATE(played_at),'YYYY-MM-DD'), COUNT(*),
			       COALESCE(SUM(duration_sec),0)/3600
			FROM listens
			WHERE played_at > NOW() - make_interval(days => $1)
			GROUP BY DATE(played_at) ORDER BY 1`, days)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		out := []fiber.Map{}
		for rows.Next() {
			var d string
			var cnt, hours int
			if rows.Scan(&d, &cnt, &hours) == nil {
				out = append(out, fiber.Map{"date": d, "count": cnt, "hours": hours})
			}
		}
		return c.JSON(fiber.Map{"days": days, "data": out})
	})

	app.Get("/listens/stats/hourly", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		rows, err := db.Query(ctx, `
			SELECT EXTRACT(DOW FROM played_at)::int, EXTRACT(HOUR FROM played_at)::int, COUNT(*)
			FROM listens GROUP BY 1,2`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		grid := make([][]int, 7)
		for i := range grid {
			grid[i] = make([]int, 24)
		}
		for rows.Next() {
			var dow, hour, cnt int
			if rows.Scan(&dow, &hour, &cnt) == nil && dow >= 0 && dow < 7 && hour >= 0 && hour < 24 {
				grid[dow][hour] = cnt
			}
		}
		return c.JSON(fiber.Map{"data": grid})
	})

	statsTop := func(field string) fiber.Handler {
		return func(c fiber.Ctx) error {
			if db == nil {
				return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
			}
			days := periodDays(c.Query("period", "month"))
			rows, err := db.Query(ctx, `
				SELECT COALESCE(NULLIF(`+field+`,''),'?') AS name, COUNT(*) AS cnt
				FROM listens
				WHERE played_at > NOW() - make_interval(days => $1)
				GROUP BY name ORDER BY cnt DESC LIMIT 10`, days)
			if err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
			defer rows.Close()
			out := []fiber.Map{}
			for rows.Next() {
				var name string
				var cnt int
				if rows.Scan(&name, &cnt) == nil {
					out = append(out, fiber.Map{"name": name, "count": cnt})
				}
			}
			return c.JSON(fiber.Map{"period": c.Query("period", "month"), "items": out})
		}
	}
	app.Get("/listens/stats/artists", statsTop("artist"))
	app.Get("/listens/stats/albums", statsTop("album"))
	app.Get("/listens/stats/tracks", statsTop("title"))

	app.Get("/listens/stats/yearly", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		rows, err := db.Query(ctx, `
			SELECT TO_CHAR(DATE_TRUNC('month', played_at),'YYYY-MM'), COUNT(*)
			FROM listens GROUP BY DATE_TRUNC('month', played_at) ORDER BY 1`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		out := []fiber.Map{}
		for rows.Next() {
			var m string
			var cnt int
			if rows.Scan(&m, &cnt) == nil {
				out = append(out, fiber.Map{"month": m, "count": cnt})
			}
		}
		return c.JSON(fiber.Map{"data": out})
	})

	app.Get("/listens/stats/heatmap", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		rows, err := db.Query(ctx, `
			SELECT EXTRACT(DOW FROM played_at)::int, EXTRACT(HOUR FROM played_at)::int, COUNT(*)
			FROM listens GROUP BY 1,2`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		grid := make([][]int, 7)
		for i := range grid {
			grid[i] = make([]int, 24)
		}
		for rows.Next() {
			var dow, hour, cnt int
			if rows.Scan(&dow, &hour, &cnt) == nil && dow >= 0 && dow < 7 && hour >= 0 && hour < 24 {
				grid[dow][hour] = cnt
			}
		}
		return c.JSON(fiber.Map{"data": grid})
	})

	app.Get("/listens/stats/compare", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		var cur, prev int
		_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM listens WHERE played_at > NOW() - INTERVAL '30 days'`).Scan(&cur)
		_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM listens WHERE played_at BETWEEN NOW() - INTERVAL '60 days' AND NOW() - INTERVAL '30 days'`).Scan(&prev)
		delta := 0
		if prev > 0 {
			delta = (cur - prev) * 100 / prev
		}
		return c.JSON(fiber.Map{"current_30d": cur, "previous_30d": prev, "delta_percent": delta})
	})

	app.Delete("/listens", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		if _, err := db.Exec(ctx, `DELETE FROM listens`); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "cleared"})
	})
}

func periodDays(p string) int {
	switch p {
	case "week":
		return 7
	case "year":
		return 365
	default:
		return 30
	}
}

// recordListen пишет старт трека в историю и возвращает id записи.
func recordListen(file, artist, album, title string, duration float64, prevID int64, prevElapsed, prevDuration float64) int64 {
	if db == nil || file == "" {
		return 0
	}
	// Только музыкальные файлы (исключаем видео и пр.).
	if !audioExtsSmart[strings.ToLower(filepath.Ext(file))] {
		return 0
	}
	base := filepath.Base(file)
	low := strings.ToLower(base)
	if strings.Contains(low, "test") || strings.Contains(low, "sample") {
		return 0
	}
	// Fallback заголовка из имени файла.
	if strings.TrimSpace(title) == "" {
		title = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if prevID > 0 && prevDuration > 0 && prevElapsed >= 0.9*prevDuration {
		_, _ = db.Exec(ctx, `UPDATE listens SET completed=true WHERE id=$1`, prevID)
	}
	var id int64
	err := db.QueryRow(ctx, `
		INSERT INTO listens (track_path, artist, album, title, duration_sec)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		file, strings.ToValidUTF8(artist, "?"), strings.ToValidUTF8(album, "?"),
		strings.ToValidUTF8(title, "?"), int(duration)).Scan(&id)
	if err != nil {
		log.Printf("⚠️ listens insert: %v", err)
		return 0
	}
	return id
}
