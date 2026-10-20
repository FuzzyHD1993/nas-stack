package main

import (
	"context"
	"errors"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
)

// dbTimeout ограничивает запросы к PostgreSQL: зависший запрос не должен
// удерживать HTTP-хендлер (и соединение из пула) бесконечно.
const dbTimeout = 10 * time.Second

func dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, dbTimeout)
}

// migrateListens создаёт таблицу истории прослушиваний.
func migrateListens() error {
	mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := db.Exec(mctx, `
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
		CREATE INDEX IF NOT EXISTS idx_listens_track_played ON listens(track_path, played_at DESC);
		CREATE INDEX IF NOT EXISTS idx_listens_artist ON listens(artist);
	`)
	return err
}

// parseFloat парсит числовой тег MPD. NaN/±Inf, отрицательные и абсурдные
// значения → 0: они попадают в int() и в INT-колонку duration_sec.
func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 24*3600 {
		return 0
	}
	return f
}

// dbFail логирует детали и не отдаёт клиенту текст ошибки PostgreSQL.
func dbFail(c fiber.Ctx, op string, err error) error {
	log.Printf("listens: %s: %v", op, err)
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

// ---------- индексация музыкальной библиотеки (с кэшем) ----------

type musicFile struct {
	Path    string
	ModTime time.Time
}

var (
	musicIdxMu   sync.RWMutex
	musicScanMu  sync.Mutex // сериализует только перестройку индекса
	musicIdx     []musicFile
	musicIdxTime time.Time
)

// musicIndex возвращает копию снимка. Мьютекс чтения удерживается только на
// копирование: обход NAS не блокирует читателей. Ошибка обхода не затирает
// прошлый удачный снимок.
func musicIndex() []musicFile {
	musicIdxMu.RLock()
	idx, ts := musicIdx, musicIdxTime
	musicIdxMu.RUnlock()
	if idx != nil && time.Since(ts) < 10*time.Minute {
		return slices.Clone(idx)
	}

	musicScanMu.Lock()
	defer musicScanMu.Unlock()

	// Пока ждали musicScanMu, другой горутин мог уже обновить снимок.
	musicIdxMu.RLock()
	idx, ts = musicIdx, musicIdxTime
	musicIdxMu.RUnlock()
	if idx != nil && time.Since(ts) < 10*time.Minute {
		return slices.Clone(idx)
	}

	base, ok := musicRoot()
	if !ok {
		return slices.Clone(idx)
	}
	files := make([]musicFile, 0, 4096)
	walkErr := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			log.Printf("musicIndex: %v", err)
			return nil // недоступный подкаталог не отменяет весь скан
		}
		if d.IsDir() {
			return nil
		}
		if !audioExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil
		}
		files = append(files, musicFile{Path: filepath.ToSlash(rel), ModTime: info.ModTime()})
		return nil
	})
	if walkErr != nil {
		log.Printf("musicIndex: walk %q: %v", base, walkErr)
		return slices.Clone(idx)
	}

	musicIdxMu.Lock()
	musicIdx, musicIdxTime = files, time.Now()
	musicIdxMu.Unlock()
	return slices.Clone(files)
}

func trackMap(path, artist, album, title string) fiber.Map {
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return fiber.Map{"track_path": path, "artist": artist, "album": album, "title": title}
}

// ---------- умные плейлисты ----------

// smartRecent — «недавно слушал»: последние уникальные треки из истории.
// Если истории ещё нет (или БД недоступна), откатываемся на mtime файлов.
func smartRecent(limit int) ([]fiber.Map, error) {
	if db != nil {
		qctx, cancel := dbCtx()
		defer cancel()
		rows, err := db.Query(qctx, `
			SELECT track_path, artist, album, title FROM (
				SELECT DISTINCT ON (track_path)
				       track_path, COALESCE(artist,'') AS artist,
				       COALESCE(album,'') AS album, COALESCE(title,'') AS title, played_at
				FROM listens
				WHERE played_at > NOW() - INTERVAL '90 days'
				ORDER BY track_path, played_at DESC
			) t
			ORDER BY played_at DESC
			LIMIT $1`, limit)
		if err == nil {
			out := []fiber.Map{}
			for rows.Next() {
				var p, a, al, t string
				if err := rows.Scan(&p, &a, &al, &t); err != nil {
					rows.Close()
					return nil, err
				}
				out = append(out, trackMap(p, a, al, t))
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
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
	qctx, cancel := dbCtx()
	defer cancel()
	rows, err := db.Query(qctx, `
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func smartForgotten(limit int) ([]fiber.Map, error) {
	last := map[string]time.Time{}
	if db != nil {
		qctx, cancel := dbCtx()
		defer cancel()
		rows, err := db.Query(qctx, `
			SELECT track_path, MAX(played_at)
			FROM listens
			WHERE played_at IS NOT NULL
			GROUP BY track_path`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			var ts time.Time
			if err := rows.Scan(&p, &ts); err != nil {
				return nil, err
			}
			last[p] = ts
		}
		if err := rows.Err(); err != nil {
			return nil, err
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
	qctx, cancel := dbCtx()
	defer cancel()

	// Последние 50 прослушиваний → любимые артисты/альбомы.
	artists := map[string]bool{}
	albums := map[string]bool{}
	rows, err := db.Query(qctx, `
		SELECT COALESCE(artist,''), COALESCE(album,'') FROM listens
		ORDER BY played_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, al string
		if err := rows.Scan(&a, &al); err != nil {
			rows.Close()
			return nil, err
		}
		if a != "" {
			artists[strings.ToLower(strings.TrimSpace(a))] = true
		}
		if al != "" {
			albums[strings.ToLower(strings.TrimSpace(al))] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	// Что слушали за 14 дней (исключаем).
	recent := map[string]bool{}
	rows, err = db.Query(qctx, `SELECT DISTINCT track_path FROM listens WHERE played_at > NOW() - INTERVAL '14 days'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		recent[p] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	// Кандидаты: треки тех же артистов/альбомов. Сравниваем компоненты пути
	// целиком, а не подстроку: "AC/DC" и "The" иначе матчили не то.
	pool := []fiber.Map{}
	for _, f := range musicIndex() {
		if recent[f.Path] {
			continue
		}
		parts := strings.Split(strings.ToLower(f.Path), "/")
		if len(parts) < 2 {
			continue
		}
		match := false
		for _, seg := range parts[:len(parts)-1] {
			if artists[seg] {
				match = true
				break
			}
		}
		if !match && albums[parts[len(parts)-2]] {
			match = true
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
			return dbFail(c, "smart playlists", err)
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
		qctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()
		rows, err := db.Query(qctx, `
			SELECT track_path, COALESCE(artist,''), COALESCE(album,''), COALESCE(title,''),
			       COALESCE(duration_sec,0), completed, played_at
			FROM listens ORDER BY played_at DESC LIMIT $1`, limit)
		if err != nil {
			return dbFail(c, "listens", err)
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
		if err := rows.Err(); err != nil {
			return dbFail(c, "listens rows", err)
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
		qctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()
		rows, err := db.Query(qctx, `
			SELECT TO_CHAR(DATE(played_at),'YYYY-MM-DD'), COUNT(*),
			       COALESCE(SUM(duration_sec),0)::float8/3600
			FROM listens
			WHERE played_at > NOW() - make_interval(days => $1)
			GROUP BY DATE(played_at) ORDER BY 1`, days)
		if err != nil {
			return dbFail(c, "daily stats", err)
		}
		defer rows.Close()
		out := []fiber.Map{}
		for rows.Next() {
			var d string
			var cnt int
			var hours float64
			if rows.Scan(&d, &cnt, &hours) == nil {
				out = append(out, fiber.Map{"date": d, "count": cnt,
					"hours": math.Round(hours*10) / 10})
			}
		}
		if err := rows.Err(); err != nil {
			return dbFail(c, "daily stats rows", err)
		}
		return c.JSON(fiber.Map{"days": days, "data": out})
	})

	// hourly и heatmap — одна и та же матрица 7×24; регистрируем один обработчик.
	heatmap := func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		days := 365
		if v, err := strconv.Atoi(c.Query("days", "365")); err == nil && v > 0 && v <= 3650 {
			days = v
		}
		qctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()
		rows, err := db.Query(qctx, `
			SELECT ((EXTRACT(DOW FROM played_at)::int + 6) % 7),
			       EXTRACT(HOUR FROM played_at)::int, COUNT(*)
			FROM listens
			WHERE played_at > NOW() - make_interval(days => $1)
			GROUP BY 1,2`, days)
		if err != nil {
			return dbFail(c, "heatmap", err)
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
		if err := rows.Err(); err != nil {
			return dbFail(c, "heatmap rows", err)
		}
		return c.JSON(fiber.Map{"days": days, "data": grid})
	}
	app.Get("/listens/stats/hourly", heatmap)

	// Белый список колонок: field приходит из кода, но не должен попадать в SQL.
	statsTopFields := map[string]string{"artist": "artist", "album": "album", "title": "title"}
	statsTop := func(field string) fiber.Handler {
		col, ok := statsTopFields[field]
		if !ok {
			panic("statsTop: unknown field " + field)
		}
		return func(c fiber.Ctx) error {
			if db == nil {
				return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
			}
			period := c.Query("period", "month")
			switch period {
			case "week", "month", "year":
			default:
				period = "month"
			}
			days := periodDays(period)
			qctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
			defer cancel()
			rows, err := db.Query(qctx, `
				SELECT COALESCE(NULLIF(`+col+`,''),'?') AS name, COUNT(*) AS cnt
				FROM listens
				WHERE played_at > NOW() - make_interval(days => $1)
				GROUP BY name ORDER BY cnt DESC LIMIT 10`, days)
			if err != nil {
				return dbFail(c, "stats top", err)
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
			if err := rows.Err(); err != nil {
				return dbFail(c, "stats top rows", err)
			}
			return c.JSON(fiber.Map{"period": period, "items": out})
		}
	}
	app.Get("/listens/stats/artists", statsTop("artist"))
	app.Get("/listens/stats/albums", statsTop("album"))
	app.Get("/listens/stats/tracks", statsTop("title"))

	app.Get("/listens/stats/yearly", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		days := 1825
		if v, err := strconv.Atoi(c.Query("days", "1825")); err == nil && v > 0 && v <= 36500 {
			days = v
		}
		qctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()
		rows, err := db.Query(qctx, `
			SELECT TO_CHAR(DATE_TRUNC('month', played_at),'YYYY-MM'), COUNT(*)
			FROM listens
			WHERE played_at > NOW() - make_interval(days => $1)
			GROUP BY DATE_TRUNC('month', played_at) ORDER BY 1`, days)
		if err != nil {
			return dbFail(c, "yearly stats", err)
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
		if err := rows.Err(); err != nil {
			return dbFail(c, "yearly stats rows", err)
		}
		return c.JSON(fiber.Map{"data": out})
	})

	app.Get("/listens/stats/heatmap", heatmap)

	app.Get("/listens/stats/compare", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		qctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()
		var cur, prev int
		if err := db.QueryRow(qctx, `SELECT COUNT(*) FROM listens WHERE played_at > NOW() - INTERVAL '30 days'`).Scan(&cur); err != nil {
			return dbFail(c, "compare current", err)
		}
		if err := db.QueryRow(qctx, `SELECT COUNT(*) FROM listens WHERE played_at BETWEEN NOW() - INTERVAL '60 days' AND NOW() - INTERVAL '30 days'`).Scan(&prev); err != nil {
			return dbFail(c, "compare previous", err)
		}
		delta := 0.0
		if prev > 0 {
			delta = math.Round(float64(cur-prev)*1000/float64(prev)) / 10
		}
		return c.JSON(fiber.Map{"current_30d": cur, "previous_30d": prev, "delta_percent": delta})
	})

	app.Delete("/listens", func(c fiber.Ctx) error {
		if c.Query("confirm") != "yes" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "confirm=yes required"})
		}
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		qctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
		defer cancel()
		if _, err := db.Exec(qctx, `DELETE FROM listens`); err != nil {
			return dbFail(c, "clear listens", err)
		}
		log.Printf("listens: history cleared by %s", c.IP())
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

// clampTag нормализует тег: валидный UTF-8, без управляющих символов,
// не длиннее max рун.
func clampTag(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 512 {
		s = strings.TrimSpace(string(r[:512]))
	}
	return s
}

// cleanListenPath: только путь внутри библиотеки, без схем, управляющих
// символов и выхода за MUSIC_DIR.
func cleanListenPath(p string) (string, bool) {
	if p == "" || len(p) > 4096 || strings.Contains(p, "://") ||
		strings.ContainsAny(p, "\r\n\x00") {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if filepath.IsAbs(filepath.FromSlash(clean)) {
		return relMusicPath(clean)
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// isIgnoredTrack отбрасывает служебные файлы и каталоги, но не альбомы
// вроде "Testament", "Greatest Hits" или "Contest": сравниваем компоненты
// пути, а не подстроку в названии.
func isIgnoredTrack(rel string) bool {
	parts := strings.Split(strings.ToLower(filepath.ToSlash(rel)), "/")
	for _, p := range parts {
		switch p {
		case "test", "tests", "sample", "samples", "preview", "previews", "temp", "tmp":
			return true
		}
		if strings.HasPrefix(p, "test_") || strings.HasPrefix(p, "sample_") ||
			strings.HasPrefix(p, "preview_") || strings.HasSuffix(p, ".tmp") ||
			strings.HasPrefix(p, ".") {
			return true
		}
	}
	base := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))
	switch base {
	case "test", "sample", "preview":
		return true
	}
	return false
}

// recordListen пишет старт трека в историю и возвращает id записи.
func recordListen(file, artist, album, title string, duration float64, prevID int64, prevElapsed, prevDuration float64) int64 {
	if db == nil {
		return 0
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Закрываем предыдущее прослушивание ДО фильтрации текущего: пропуск
	// служебного файла не должен навсегда оставить completed=false.
	if prevID > 0 && prevDuration > 0 && prevElapsed >= 0.9*prevDuration {
		if _, err := db.Exec(qctx,
			`UPDATE listens SET completed=true WHERE id=$1 AND completed=false`, prevID); err != nil {
			log.Printf("listens: mark completed %d: %v", prevID, err)
		}
	}

	rel, ok := cleanListenPath(file)
	if !ok {
		return 0
	}
	// Только музыкальные файлы (исключаем видео и пр.) и не служебные.
	if !audioExts[strings.ToLower(filepath.Ext(rel))] || isIgnoredTrack(rel) {
		return 0
	}
	// Fallback заголовка из имени файла.
	artist, album, title = clampTag(artist), clampTag(album), clampTag(title)
	base := filepath.Base(rel)
	if title == "" {
		title = strings.TrimSuffix(base, filepath.Ext(base))
	}
	dur := 0
	if !math.IsNaN(duration) && !math.IsInf(duration, 0) && duration >= 0 && duration <= 24*3600 {
		dur = int(duration)
	}

	// Окно дедупликации: повторный play/seek того же файла (рестарт,
	// возврат назад) не создаёт вторую строку.
	var id int64
	err := db.QueryRow(qctx, `
		INSERT INTO listens (track_path, artist, album, title, duration_sec)
		SELECT $1,$2,$3,$4,$5
		WHERE NOT EXISTS (
			SELECT 1 FROM listens
			WHERE track_path = $1 AND played_at > NOW() - INTERVAL '60 seconds'
		)
		RETURNING id`,
		rel, artist, album, title, dur).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0 // окно дедупликации — не ошибка
	}
	if err != nil {
		log.Printf("⚠️ listens insert: %v", err)
		return 0
	}
	return id
}
