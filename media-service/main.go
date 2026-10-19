package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/fsnotify/fsnotify"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

var (
	db  *pgxpool.Pool
	rdb *redis.Client
	ctx = context.Background()
)

var tagsSet = map[string]bool{
	// Качество
	"1080p": true, "720p": true, "2160p": true, "4k": true, "uhd": true,
	"hdr": true, "sdr": true, "hdr10": true, "dv": true, "dovi": true,
	"10bit": true, "8bit": true,
	// Источник
	"web-dl": true, "webrip": true, "webdl": true, "bdrip": true,
	"bdremux": true, "bd-remux": true, "bluray": true, "blu-ray": true,
	"hdtv": true, "dvdrip": true, "hdrip": true, "remux": true, "ts": true,
	// Кодек
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true,
	"avc": true, "avc1": true, "xvid": true, "divx": true,
	// Аудио
	"aac": true, "ac3": true, "dts": true, "dts-hd": true, "truehd": true,
	"atmos": true, "dd5.1": true, "dd2.0": true, "ddp5.1": true,
	// Группы
	"r.g.": true, "rarbg": true, "yts": true, "yify": true, "hdclub": true,
	"selezen": true, "nnmclub": true, "kurazh-bambej": true, "exkinoray": true,
	"eniahd": true, "newcomers": true, "lostfilm": true, "newstudio": true,
	"files-x": true, "25kuzmich": true, "mrmittens": true, "ylnian": true,
	"whip93": true, "casstudio": true, "uniongang": true, "teamhd": true,
	"д": true, "p": true, "b": true,
	// Маркеры
	"от": true, "mvo": true, "avo": true, "p2": true, "l1": true,
	"rus": true, "eng": true,
}

var (
	seasonRe = regexp.MustCompile(`^[sс]\d{1,2}([eе]\d{1,3})?$|^[eе]\d{1,3}$|^(?:сезон|season)\s*\d+$`)
	prefixRe = regexp.MustCompile(`^\d+\s+`)
	yearRe   = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)

	epSE      = regexp.MustCompile(`(?i)s(\d{1,2})e(\d{1,2})`)
	epX       = regexp.MustCompile(`(?i)\b(\d{1,2})[xх](\d{1,2})\b`)
	epSeason  = regexp.MustCompile(`(?i)сезон[\s._-]*(\d{1,2}).*?сери[яи][\s._-]*(\d{1,2})`)
	epCyr     = regexp.MustCompile(`(?i)с(\d{1,2})е(\d{1,2})`)
	epBracket = regexp.MustCompile(`(?i)\[(\d{1,2})[xх](\d{1,2})\]`)
	epLeading = regexp.MustCompile(`^(\d{1,3})[\s._-]`)
	seasonDir = regexp.MustCompile(`(?i)[sс](\d{1,2})\b`)
)

type mediaItem struct {
	ID            int         `json:"id"`
	MediaType     string      `json:"media_type"`
	Title         string      `json:"title"`
	OriginalTitle string      `json:"original_title"`
	Year          int         `json:"year"`
	TMDBID        int         `json:"tmdb_id"`
	Overview      string      `json:"overview"`
	PosterURL     string      `json:"poster_url"`
	BackdropURL   string      `json:"backdrop_url"`
	Rating        float64     `json:"rating"`
	Genres        string      `json:"genres"`
	Runtime       int         `json:"runtime"`
	AudioCodec    string      `json:"audio_codec,omitempty"`
	VideoCodec    string      `json:"video_codec,omitempty"`
	TrailerURL    string      `json:"trailer_url,omitempty"`
	Actors        []personRef `json:"actors,omitempty"`
	Director      *personRef  `json:"director,omitempty"`
	Path          string      `json:"path,omitempty"`
}

// personRef — краткая карточка персоны для выдачи.
type personRef struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	ProfileURL string `json:"profile_url"`
	Character  string `json:"character,omitempty"`
}

const mediaCols = `id, media_type, title, original_title, year, tmdb_id, overview, poster_url, backdrop_url, rating, genres, runtime`

// Расширения для фото-галереи.
var photoImageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".webp": true, ".bmp": true, ".heic": true, ".heif": true,
}

var photoVideoExts = map[string]bool{
	".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".m4v": true, ".webm": true,
}

// photoAbsPath превращает относительный путь в абсолютный внутри MEDIA_PHOTOS_DIR,
// защищаясь от выхода за пределы каталога.
func photoAbsPath(rel string) (string, bool) {
	dir := platform.GetEnv("MEDIA_PHOTOS_DIR", "/photos")
	root := filepath.Clean(dir)
	clean := filepath.Clean(filepath.Join(root, rel))
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return "", false
	}
	return clean, true
}

func main() {
	if err := platform.ValidateEnv([]string{"TMDB_API_KEY", "POSTGRES_URL"}); err != nil {
		log.Fatalf("❌ %v", err)
	}
	initPhotoCache()
	pgURL := platform.GetEnv("POSTGRES_URL", "postgres://nas:nas_secret@localhost:5432/nas_history?sslmode=disable")
	var err error
	db, err = pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatalf("❌ PostgreSQL: %v", err)
	}
	defer db.Close()

	if err := migrate(); err != nil {
		log.Fatalf("❌ Migration: %v", err)
	}

	rdb = redis.NewClient(&redis.Options{
		Addr: platform.GetEnv("REDIS_ADDR", "localhost:6379"),
	})

	go startAutoScan()
	go startWatchdog()

	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error {
		if err := db.Ping(ctx); err != nil {
			return c.Status(500).JSON(fiber.Map{"status": "error", "db": err.Error()})
		}
		return c.JSON(platform.HealthJSON("media", nil))
	})

	// Сканировать библиотеку (фоново)
	app.Post("/scan", func(c fiber.Ctx) error {
		ok, err := rdb.SetNX(ctx, "media:scan:running", "true", 10*time.Minute).Result()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if !ok {
			return c.JSON(fiber.Map{"status": "already_running"})
		}
		_ = rdb.Set(ctx, "media:scan:progress", "0", 10*time.Minute).Err()
		_ = rdb.Set(ctx, "media:scan:total", "0", 10*time.Minute).Err()
		go runScan()
		return c.JSON(fiber.Map{"status": "started"})
	})

	// Статус сканирования
	app.Get("/scan/status", func(c fiber.Ctx) error {
		running := rdb.Get(ctx, "media:scan:running").Val() == "true"
		progress, _ := strconv.Atoi(rdb.Get(ctx, "media:scan:progress").Val())
		total, _ := strconv.Atoi(rdb.Get(ctx, "media:scan:total").Val())
		last := rdb.Get(ctx, "media:scan:last").Val()
		result := map[string]interface{}{}
		if rs := rdb.Get(ctx, "media:scan:result").Val(); rs != "" {
			_ = json.Unmarshal([]byte(rs), &result)
		}
		return c.JSON(fiber.Map{"running": running, "progress": progress, "total": total, "last": last, "result": result})
	})

	// Дозагрузка актёров/режиссёров для уже просканированных медиа
	app.Post("/scan/credits", func(c fiber.Ctx) error {
		ok, err := rdb.SetNX(ctx, "media:credits:running", "true", 30*time.Minute).Result()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if !ok {
			return c.JSON(fiber.Map{"status": "already_running"})
		}
		go func() {
			defer rdb.Del(ctx, "media:credits:running")
			rows, err := db.Query(ctx, `
				SELECT m.id, m.media_type, m.tmdb_id FROM media m
				WHERE m.tmdb_id > 0
				  AND NOT EXISTS (SELECT 1 FROM media_people mp WHERE mp.media_id = m.id)`)
			if err != nil {
				return
			}
			type job struct {
				id        int
				mediaType string
				tmdbID    int
			}
			jobs := []job{}
			for rows.Next() {
				var j job
				if rows.Scan(&j.id, &j.mediaType, &j.tmdbID) == nil {
					jobs = append(jobs, j)
				}
			}
			rows.Close()
			log.Printf("🎭 Дозагрузка актёров: %d медиа", len(jobs))
			for _, j := range jobs {
				syncCredits(j.id, j.mediaType, j.tmdbID)
				time.Sleep(250 * time.Millisecond)
			}
			log.Printf("🎭 Дозагрузка актёров завершена")
		}()
		return c.JSON(fiber.Map{"status": "started"})
	})

	// Watchdog: счётчик новых файлов
	app.Get("/new/count", func(c fiber.Ctx) error {
		n, _ := rdb.Get(ctx, "media:new_count").Int()
		return c.JSON(fiber.Map{"count": n})
	})
	app.Post("/new/reset", func(c fiber.Ctx) error {
		_ = rdb.Set(ctx, "media:new_count", "0", 0).Err()
		return c.JSON(fiber.Map{"status": "ok", "count": 0})
	})

	// Фильмы
	app.Get("/movies", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"items": queryMedia("movie", parseMediaFilter(c)), "type": "movie"})
	})
	app.Get("/movies/:id", func(c fiber.Ctx) error {
		return mediaDetail(c)
	})

	// Дубликаты в библиотеке (по tmdb_id).
	app.Get("/duplicates", func(c fiber.Ctx) error {
		rows, err := db.Query(ctx, `
			SELECT tmdb_id FROM media WHERE tmdb_id IS NOT NULL AND tmdb_id > 0
			GROUP BY tmdb_id HAVING COUNT(*) > 1 ORDER BY COUNT(*) DESC`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		var ids []int
		for rows.Next() {
			var id int
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		dupes := []fiber.Map{}
		for _, tid := range ids {
			vrows, err := db.Query(ctx, `SELECT id, title, COALESCE(path,'') FROM media WHERE tmdb_id=$1 ORDER BY id`, tid)
			if err != nil {
				continue
			}
			versions := []fiber.Map{}
			title := ""
			for vrows.Next() {
				var id int
				var t, p string
				if vrows.Scan(&id, &t, &p) == nil {
					if title == "" {
						title = t
					}
					res := "unknown"
					lp := strings.ToLower(p)
					switch {
					case strings.Contains(lp, "2160p") || strings.Contains(lp, "4k"):
						res = "2160p"
					case strings.Contains(lp, "1080p"):
						res = "1080p"
					case strings.Contains(lp, "720p"):
						res = "720p"
					case strings.Contains(lp, "480p"):
						res = "480p"
					}
					var size int64
					if fi, e := os.Stat(p); e == nil {
						size = fi.Size()
					}
					versions = append(versions, fiber.Map{"id": id, "path": p, "resolution": res, "size_bytes": size})
				}
			}
			vrows.Close()
			dupes = append(dupes, fiber.Map{"tmdb_id": tid, "title": title, "versions": versions})
		}
		return c.JSON(fiber.Map{"duplicates": dupes, "count": len(dupes)})
	})

	app.Post("/duplicates/resolve", func(c fiber.Ctx) error {
		var req struct {
			TmdbID    int   `json:"tmdb_id"`
			KeepID    int   `json:"keep_id"`
			DeleteIDs []int `json:"delete_ids"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "bad json"})
		}
		agentURL := platform.GetEnv("HOST_AGENT_URL", "http://host.containers.internal:9998")
		hostPaths := []string{}
		deleteSet := map[int]bool{}
		for _, id := range req.DeleteIDs {
			if id == req.KeepID {
				return c.Status(400).JSON(fiber.Map{"error": "cannot delete keep_id"})
			}
			var p, mt string
			if db.QueryRow(ctx, `SELECT COALESCE(path,''), media_type FROM media WHERE id=$1`, id).Scan(&p, &mt) == nil && p != "" {
				hostPaths = append(hostPaths, toHostPath(p, mt))
				deleteSet[id] = true
			}
		}
		var keepHost string
		{
			var p, mt string
			if db.QueryRow(ctx, `SELECT COALESCE(path,''), media_type FROM media WHERE id=$1`, req.KeepID).Scan(&p, &mt) == nil {
				keepHost = toHostPath(p, mt)
			}
		}
		moved := []string{}
		if len(hostPaths) > 0 {
			body, _ := json.Marshal(fiber.Map{"paths": hostPaths, "keep": keepHost})
			req2, _ := http.NewRequest("POST", agentURL+"/duplicates/resolve", bytes.NewReader(body))
			req2.Header.Set("Content-Type", "application/json")
			// host-agent защищает перенос файлов библиотеки ADMIN_TOKEN'ом.
			if t := strings.TrimSpace(platform.GetEnv("ADMIN_TOKEN", "")); t != "" {
				req2.Header.Set("X-Admin-Token", t)
			}
			hc := &http.Client{Timeout: 60 * time.Second}
			resp, err := hc.Do(req2)
			if err != nil {
				return c.Status(502).JSON(fiber.Map{"error": "host-agent: " + err.Error()})
			}
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
			_ = resp.Body.Close()
			var ar struct {
				Moved []string `json:"moved"`
			}
			_ = json.Unmarshal(respBody, &ar)
			moved = ar.Moved
		}
		// Удаляем из библиотеки только те записи, чьи файлы перемещены.
		removed := 0
		movedSet := map[string]bool{}
		for _, m := range moved {
			movedSet[m] = true
		}
		for id := range deleteSet {
			var p, mt string
			_ = db.QueryRow(ctx, `SELECT COALESCE(path,''), media_type FROM media WHERE id=$1`, id).Scan(&p, &mt)
			if movedSet[toHostPath(p, mt)] {
				if ct, err := db.Exec(ctx, `DELETE FROM media WHERE id=$1`, id); err == nil {
					removed += int(ct.RowsAffected())
				}
			}
		}
		return c.JSON(fiber.Map{"status": "resolved", "removed": removed, "moved_files": len(moved)})
	})

	// Умный поиск фильмов по параметрам (жанр/длительность/рейтинг/год).
	app.Post("/movies/smart", func(c fiber.Ctx) error {
		var req struct {
			Genre      string  `json:"genre"`
			RuntimeMax int     `json:"runtime_max"`
			RuntimeMin int     `json:"runtime_min"`
			RatingMin  float64 `json:"rating_min"`
			YearFrom   int     `json:"year_from"`
			YearTo     int     `json:"year_to"`
			Limit      int     `json:"limit"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "bad json"})
		}
		if req.Limit <= 0 || req.Limit > 200 {
			req.Limit = 50
		}
		query := `SELECT ` + mediaCols + ` FROM media WHERE media_type='movie'`
		args := []interface{}{}
		i := 1
		if req.Genre != "" {
			query += ` AND LOWER(genres) LIKE '%' || $` + strconv.Itoa(i) + ` || '%'`
			args = append(args, strings.ToLower(req.Genre))
			i++
		}
		if req.RuntimeMax > 0 {
			query += ` AND runtime > 0 AND runtime <= $` + strconv.Itoa(i)
			args = append(args, req.RuntimeMax)
			i++
		}
		if req.RuntimeMin > 0 {
			query += ` AND runtime >= $` + strconv.Itoa(i)
			args = append(args, req.RuntimeMin)
			i++
		}
		if req.RatingMin > 0 {
			query += ` AND rating >= $` + strconv.Itoa(i)
			args = append(args, req.RatingMin)
			i++
		}
		if req.YearFrom > 0 {
			query += ` AND year >= $` + strconv.Itoa(i)
			args = append(args, req.YearFrom)
			i++
		}
		if req.YearTo > 0 {
			query += ` AND year <= $` + strconv.Itoa(i)
			args = append(args, req.YearTo)
			i++
		}
		query += ` ORDER BY rating DESC LIMIT $` + strconv.Itoa(i)
		args = append(args, req.Limit)
		movies := mediaList(query, args...)
		return c.JSON(fiber.Map{"movies": movies, "items": movies, "count": len(movies)})
	})

	// Сериалы
	app.Get("/tv", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"items": queryMedia("tv", parseMediaFilter(c)), "type": "tv"})
	})
	app.Get("/tv/:id", func(c fiber.Ctx) error {
		return mediaDetail(c)
	})

	// Список жанров
	app.Get("/genres", func(c fiber.Ctx) error {
		rows, err := db.Query(ctx, `SELECT DISTINCT genres FROM media WHERE genres <> ''`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		set := map[string]bool{}
		for rows.Next() {
			var g string
			if rows.Scan(&g) == nil {
				for _, part := range strings.Split(g, ",") {
					p := strings.TrimSpace(part)
					if p != "" {
						set[p] = true
					}
				}
			}
		}
		genres := []string{}
		for g := range set {
			genres = append(genres, g)
		}
		sort.Strings(genres)
		return c.JSON(fiber.Map{"genres": genres})
	})

	// Список годов
	app.Get("/years", func(c fiber.Ctx) error {
		rows, err := db.Query(ctx, `SELECT DISTINCT year FROM media WHERE year > 0 ORDER BY year DESC`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		years := []int{}
		for rows.Next() {
			var y int
			if rows.Scan(&y) == nil {
				years = append(years, y)
			}
		}
		return c.JSON(fiber.Map{"years": years})
	})

	// Прокси постера
	app.Get("/poster", func(c fiber.Ctx) error {
		p := c.Query("url")
		if p == "" {
			return c.Status(400).JSON(fiber.Map{"error": "url is required"})
		}
		if !strings.HasPrefix(p, "https://image.tmdb.org/") {
			return c.Status(400).JSON(fiber.Map{"error": "invalid url"})
		}
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Get(p)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "image/jpeg"
		}
		c.Set("Content-Type", ct)
		c.Set("Cache-Control", "public, max-age=86400")
		return c.Send(body)
	})

	// ============================================================
	// ФОТО-ГАЛЕРЕЯ
	// ============================================================

	// Список фото, сгруппированных по дате съёмки (mtime)
	app.Get("/photos/groups", func(c fiber.Ctx) error {
		dir := platform.GetEnv("MEDIA_PHOTOS_DIR", "/photos")
		type photoEntry struct {
			Path    string `json:"path"`
			Name    string `json:"name"`
			Type    string `json:"type"`
			ModTime int64  `json:"mod_time"`
			Size    int64  `json:"size"`
		}
		groupsMap := map[string][]photoEntry{}
		if _, err := os.Stat(dir); err != nil {
			return c.JSON(fiber.Map{"groups": []fiber.Map{}, "error": "photos dir unavailable"})
		}
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			kind := ""
			if photoImageExts[ext] {
				kind = "image"
			} else if photoVideoExts[ext] {
				kind = "video"
			}
			if kind == "" {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return nil
			}
			date := info.ModTime().Format("2006-01-02")
			groupsMap[date] = append(groupsMap[date], photoEntry{
				Path: rel, Name: d.Name(), Type: kind, ModTime: info.ModTime().Unix(), Size: info.Size(),
			})
			return nil
		})

		dates := make([]string, 0, len(groupsMap))
		for date := range groupsMap {
			dates = append(dates, date)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(dates)))
		groups := []fiber.Map{}
		for _, date := range dates {
			photos := groupsMap[date]
			sort.Slice(photos, func(i, j int) bool { return photos[i].Name < photos[j].Name })
			groups = append(groups, fiber.Map{"date": date, "count": len(photos), "photos": photos})
		}
		return c.JSON(fiber.Map{"groups": groups, "total": len(dates)})
	})

	// Превью фото/видео: локальный SSD-кэш (по умолчанию 400px).
	app.Get("/photos/thumbnail", func(c fiber.Ctx) error {
		rel := c.Query("path")
		if rel == "" {
			return c.Status(400).JSON(fiber.Map{"error": "path is required"})
		}
		full, ok := photoAbsPath(rel)
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid path"})
		}
		if _, err := os.Stat(full); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		size := 400
		if v, err := strconv.Atoi(c.Query("size", "400")); err == nil && v >= 64 && v <= 1600 {
			size = v
		}
		isVideo := photoVideoExts[strings.ToLower(filepath.Ext(full))]
		cachePath, err := ensurePhotoCache(rel, size, "thumb", full, isVideo)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "thumbnail failed: " + err.Error()})
		}
		c.Set("Content-Type", "image/jpeg")
		c.Set("Cache-Control", "public, max-age=86400")
		return c.SendFile(cachePath)
	})

	// Случайное фото (для фона UI)
	app.Get("/photos/random", func(c fiber.Ctx) error {
		dir := platform.GetEnv("MEDIA_PHOTOS_DIR", "/photos")
		var files []string
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !photoImageExts[strings.ToLower(filepath.Ext(p))] {
				return nil
			}
			files = append(files, p)
			return nil
		})
		if len(files) == 0 {
			return c.JSON(fiber.Map{"url": ""})
		}
		idx := int(time.Now().UnixNano()) % len(files)
		if idx < 0 {
			idx = 0
		}
		rel, err := filepath.Rel(dir, files[idx])
		if err != nil {
			return c.JSON(fiber.Map{"url": ""})
		}
		u := "/api/media/photos/thumbnail?path=" + url.QueryEscape(rel)
		return c.JSON(fiber.Map{"url": u, "name": filepath.Base(files[idx])})
	})

	// Полное фото: видео — оригинал, изображения — SSD-кэш (до 1200px).
	app.Get("/photos/full", func(c fiber.Ctx) error {
		rel := c.Query("path")
		if rel == "" {
			return c.Status(400).JSON(fiber.Map{"error": "path is required"})
		}
		full, ok := photoAbsPath(rel)
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid path"})
		}
		if _, err := os.Stat(full); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		ext := strings.ToLower(filepath.Ext(full))
		if photoVideoExts[ext] {
			return c.SendFile(full)
		}
		fullSize := 1200
		if v, err := strconv.Atoi(platform.GetEnv("PHOTO_FULL_SIZE", "1200")); err == nil && v >= 320 && v <= 4000 {
			fullSize = v
		}
		cachePath, err := ensurePhotoCache(rel, fullSize, "full", full, false)
		if err != nil {
			// Не смогли отресайзить — отдаём оригинал, чтобы фото не пропало.
			return c.SendFile(full)
		}
		c.Set("Content-Type", "image/jpeg")
		c.Set("Cache-Control", "public, max-age=86400")
		return c.SendFile(cachePath)
	})

	// Очистка локального кэша фото.
	app.Delete("/photos/cache", func(c fiber.Ctx) error {
		if err := clearPhotoCache(); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "cache_cleared"})
	})

	// Открыть в VLC
	app.Post("/open/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		if err := openMedia(id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "opened"})
	})

	// Стриминг для HTML5-плеера (Range-запросы поддерживаются SendFile)
	app.Get("/stream/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var p, mediaType string
		if err := db.QueryRow(ctx, `SELECT path, media_type FROM media WHERE id=$1`, id).Scan(&p, &mediaType); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		filePath := p
		if mediaType == "tv" {
			f, err := firstVideoFile(p)
			if err != nil {
				return c.Status(404).JSON(fiber.Map{"error": err.Error()})
			}
			filePath = f
		}
		return c.SendFile(filePath)
	})

	// Стриминг с транскодом аудио (для кодеков AC3/DTS/TrueHD)
	app.Get("/stream/transcoded/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		filePath, _, err := resolveMediaFile(id)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": err.Error()})
		}
		return streamTranscoded(c, filePath)
	})

	// Информация о необходимости транскода (для UI-бейджа)
	app.Get("/transcode/info/:id", func(c fiber.Ctx) error {
		return videoTranscodeInfo(c)
	})

	// Сезоны сериала
	app.Get("/tv/:id/seasons", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		rows, err := db.Query(ctx, `SELECT season, COUNT(*) FROM episodes WHERE series_id=$1 GROUP BY season ORDER BY season`, id)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		seasons := []fiber.Map{}
		for rows.Next() {
			var s, c int
			if rows.Scan(&s, &c) == nil {
				seasons = append(seasons, fiber.Map{"season": s, "episode_count": c})
			}
		}
		return c.JSON(fiber.Map{"seasons": seasons})
	})

	// Эпизоды сезона
	app.Get("/tv/:id/season/:season", func(c fiber.Ctx) error {
		id, _ := strconv.Atoi(c.Params("id"))
		season, _ := strconv.Atoi(c.Params("season"))
		return c.JSON(fiber.Map{"episodes": listEpisodes(id, season)})
	})

	// Детали эпизода
	app.Get("/episode/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var e episodeItem
		err = db.QueryRow(ctx, `SELECT `+episodeCols+` FROM episodes WHERE id=$1`, id).
			Scan(&e.ID, &e.SeriesID, &e.Season, &e.Episode, &e.Title, &e.Overview, &e.Runtime, &e.AirDate, &e.ThumbnailURL, &e.VideoCodec, &e.AudioCodec, &e.Path)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		return c.JSON(e)
	})

	// Стрим эпизода (Range)
	app.Get("/episode/:id/stream", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var path string
		if err := db.QueryRow(ctx, `SELECT path FROM episodes WHERE id=$1`, id).Scan(&path); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		return c.SendFile(path)
	})

	// Стрим эпизода с транскодом
	app.Get("/episode/:id/stream/transcoded", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var path string
		if err := db.QueryRow(ctx, `SELECT path FROM episodes WHERE id=$1`, id).Scan(&path); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		return streamTranscoded(c, path)
	})

	// Субтитры эпизода
	app.Get("/episode/:id/subtitles", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var path string
		if err := db.QueryRow(ctx, `SELECT path FROM episodes WHERE id=$1`, id).Scan(&path); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		sub, err := findSubtitle(path)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "субтитры не найдены"})
		}
		c.Set("Content-Type", "application/x-subrip")
		return c.SendFile(sub)
	})

	// Миниатюра эпизода
	app.Get("/episode/:id/thumbnail", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var path string
		if err := db.QueryRow(ctx, `SELECT path FROM episodes WHERE id=$1`, id).Scan(&path); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		thumb := episodeThumbnail(id, path)
		if thumb == "" {
			return c.Status(404).JSON(fiber.Map{"error": "не удалось создать миниатюру"})
		}
		c.Set("Content-Type", "image/jpeg")
		return c.SendFile(thumb)
	})

	// Открыть эпизод в VLC
	app.Post("/episode/:id/open", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		if err := openEpisode(id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "opened"})
	})

	// Следующий эпизод
	app.Get("/episode/:id/next", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var seriesID, season, episode int
		if err := db.QueryRow(ctx, `SELECT series_id, season, episode FROM episodes WHERE id=$1`, id).Scan(&seriesID, &season, &episode); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		var nid, nseason, nepisode int
		var ntitle string
		err = db.QueryRow(ctx, `
			SELECT id, season, episode, COALESCE(title,'') FROM episodes
			WHERE series_id=$1 AND ((season=$2 AND episode>$3) OR season>$2)
			ORDER BY season ASC, episode ASC LIMIT 1`, seriesID, season, episode).Scan(&nid, &nseason, &nepisode, &ntitle)
		if err != nil {
			return c.JSON(fiber.Map{"next": nil})
		}
		return c.JSON(fiber.Map{"next": fiber.Map{"id": nid, "season": nseason, "episode": nepisode, "title": ntitle}})
	})

	// Список субтитров эпизода
	app.Get("/subtitles/list/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var path string
		if err := db.QueryRow(ctx, `SELECT path FROM episodes WHERE id=$1`, id).Scan(&path); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		subs := collectSubtitles(path)
		list := []fiber.Map{}
		for _, s := range subs {
			list = append(list, fiber.Map{"idx": s.idx, "lang": s.lang, "label": s.label, "forced": s.forced})
		}
		return c.JSON(fiber.Map{"subtitles": list})
	})

	// Отдать субтитр (с конвертацией .ass/.sub → .vtt)
	app.Get("/subtitles/:id/:idx", func(c fiber.Ctx) error {
		id, _ := strconv.Atoi(c.Params("id"))
		idx, _ := strconv.Atoi(c.Params("idx"))
		var path string
		if err := db.QueryRow(ctx, `SELECT path FROM episodes WHERE id=$1`, id).Scan(&path); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		subs := collectSubtitles(path)
		var target string
		for _, s := range subs {
			if s.idx == idx {
				target = s.path
				break
			}
		}
		if target == "" {
			return c.Status(404).JSON(fiber.Map{"error": "нет субтитра"})
		}
		ext := strings.ToLower(filepath.Ext(target))
		if ext == ".ass" || ext == ".sub" {
			cached := fmt.Sprintf("/tmp/sub-%d-%d.vtt", id, idx)
			if _, err := os.Stat(cached); os.IsNotExist(err) {
				_ = exec.Command("ffmpeg", "-y", "-i", target, cached).Run()
			}
			if _, err := os.Stat(cached); err == nil {
				c.Set("Content-Type", "text/vtt")
				return c.SendFile(cached)
			}
		}
		if ext == ".vtt" {
			c.Set("Content-Type", "text/vtt")
		} else {
			c.Set("Content-Type", "application/x-subrip")
		}
		return c.SendFile(target)
	})

	// Статистика
	app.Get("/stats", func(c fiber.Ctx) error {
		var movies, tv int
		_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM media WHERE media_type='movie'`).Scan(&movies)
		_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM media WHERE media_type='tv'`).Scan(&tv)
		return c.JSON(fiber.Map{"movies": movies, "tv": tv, "total": movies + tv})
	})

	// Прогресс просмотра
	app.Post("/progress", func(c fiber.Ctx) error {
		var body struct {
			MediaType   string `json:"media_type"`
			MediaID     int    `json:"media_id"`
			PositionSec int    `json:"position_sec"`
			DurationSec int    `json:"duration_sec"`
		}
		if err := c.Bind().JSON(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		if body.MediaType == "" || body.MediaID <= 0 {
			return c.Status(400).JSON(fiber.Map{"error": "нужны media_type и media_id"})
		}
		// Досмотрел (>95%) — удаляем
		if body.DurationSec > 0 && body.PositionSec >= int(float64(body.DurationSec)*0.95) {
			_, _ = db.Exec(ctx, `DELETE FROM playback_progress WHERE media_type=$1 AND media_id=$2`, body.MediaType, body.MediaID)
			return c.JSON(fiber.Map{"status": "cleared"})
		}
		_, err := db.Exec(ctx, `
			INSERT INTO playback_progress (media_type, media_id, position_sec, duration_sec, updated_at)
			VALUES ($1,$2,$3,$4,NOW())
			ON CONFLICT (media_type, media_id) DO UPDATE SET
				position_sec=EXCLUDED.position_sec, duration_sec=EXCLUDED.duration_sec, updated_at=NOW()`,
			body.MediaType, body.MediaID, body.PositionSec, body.DurationSec)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "saved"})
	})

	app.Get("/progress/:type/:id", func(c fiber.Ctx) error {
		mediaType := c.Params("type")
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var pos, dur int
		err = db.QueryRow(ctx, `SELECT position_sec, duration_sec FROM playback_progress WHERE media_type=$1 AND media_id=$2`, mediaType, id).Scan(&pos, &dur)
		if err != nil {
			return c.JSON(fiber.Map{"position_sec": 0, "duration_sec": 0, "percent": 0})
		}
		percent := 0.0
		if dur > 0 {
			percent = float64(pos) / float64(dur) * 100
		}
		return c.JSON(fiber.Map{"position_sec": pos, "duration_sec": dur, "percent": percent})
	})

	app.Get("/progress/continue", func(c fiber.Ctx) error {
		rows, err := db.Query(ctx, `
			SELECT media_type, media_id, position_sec, duration_sec
			FROM playback_progress WHERE position_sec > 30
			ORDER BY updated_at DESC LIMIT 10`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		for rows.Next() {
			var mt string
			var mid, pos, dur int
			if rows.Scan(&mt, &mid, &pos, &dur) == nil {
				percent := 0.0
				if dur > 0 {
					percent = float64(pos) / float64(dur) * 100
				}
				items = append(items, fiber.Map{"media_type": mt, "media_id": mid, "position_sec": pos, "duration_sec": dur, "percent": percent})
			}
		}
		return c.JSON(fiber.Map{"items": items})
	})

	// Поиск в TMDb для ручного сопоставления
	app.Get("/match/search", func(c fiber.Ctx) error {
		q := c.Query("q")
		mediaType := c.Query("type", "movie")
		if q == "" {
			return c.Status(400).JSON(fiber.Map{"error": "q is required"})
		}
		res, err := searchTMDB(mediaType, q, 0)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		results := []fiber.Map{}
		for _, r := range res.Results {
			title := r.Title
			if title == "" {
				title = r.Name
			}
			year := 0
			dateStr := r.ReleaseDate
			if dateStr == "" {
				dateStr = r.FirstAirDate
			}
			if len(dateStr) >= 4 {
				year, _ = strconv.Atoi(dateStr[:4])
			}
			poster := ""
			if r.PosterPath != "" {
				poster = "https://image.tmdb.org/t/p/w500" + r.PosterPath
			}
			results = append(results, fiber.Map{"tmdb_id": r.ID, "title": title, "year": year, "poster_url": poster})
		}
		return c.JSON(fiber.Map{"results": results})
	})

	// Привязать media к TMDb
	app.Post("/match/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var body struct {
			TMDBID int `json:"tmdb_id"`
		}
		if err := c.Bind().JSON(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		if body.TMDBID <= 0 {
			return c.Status(400).JSON(fiber.Map{"error": "нужен tmdb_id"})
		}
		var mediaType string
		if err := db.QueryRow(ctx, `SELECT media_type FROM media WHERE id=$1`, id).Scan(&mediaType); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		d, err := tmdbDetails(mediaType, body.TMDBID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		title := d.Title
		if title == "" {
			title = d.Name
		}
		orig := d.OriginalTitle
		if orig == "" {
			orig = d.OriginalName
		}
		poster, backdrop := "", ""
		if d.PosterPath != "" {
			poster = "https://image.tmdb.org/t/p/w500" + d.PosterPath
		}
		if d.BackdropPath != "" {
			backdrop = "https://image.tmdb.org/t/p/w1280" + d.BackdropPath
		}
		names := []string{}
		for _, g := range d.Genres {
			names = append(names, g.Name)
		}
		year := 0
		dateStr := d.ReleaseDate
		if dateStr == "" {
			dateStr = d.FirstAirDate
		}
		if len(dateStr) >= 4 {
			year, _ = strconv.Atoi(dateStr[:4])
		}
		runtime := d.Runtime
		if mediaType == "tv" && len(d.EpisodeRunTime) > 0 {
			runtime = d.EpisodeRunTime[0]
		}
		_, err = db.Exec(ctx, `
			UPDATE media SET tmdb_id=$1, title=$2, original_title=$3, year=$4, overview=$5,
				poster_url=$6, backdrop_url=$7, rating=$8, genres=$9, runtime=$10
			WHERE id=$11`,
			d.ID, title, orig, year, d.Overview, poster, backdrop, d.VoteAverage, strings.Join(names, ", "), runtime, id)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "matched"})
	})

	// Сбросить сопоставление TMDb
	app.Post("/match/:id/reset", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		_, err = db.Exec(ctx, `UPDATE media SET tmdb_id=0 WHERE id=$1`, id)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "reset"})
	})

	// Умные полки
	app.Get("/smart/recent", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"items": mediaList(`SELECT ` + mediaCols + ` FROM media ORDER BY added_at DESC LIMIT 20`)})
	})
	app.Get("/smart/top_rated", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"items": mediaList(`SELECT ` + mediaCols + ` FROM media WHERE rating > 0 ORDER BY rating DESC LIMIT 20`)})
	})
	app.Get("/smart/unwatched", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"items": mediaList(`SELECT ` + mediaCols + ` FROM media m WHERE media_type='movie' AND NOT EXISTS (SELECT 1 FROM playback_progress p WHERE p.media_type='movie' AND p.media_id=m.id) ORDER BY added_at DESC LIMIT 20`)})
	})

	// Коллекции (Marvel, Star Wars и т.д.)
	app.Get("/collections", func(c fiber.Ctx) error {
		rows, err := db.Query(ctx, `SELECT c.id, c.tmdb_id, c.name, COALESCE(c.poster_url,''), COALESCE(c.backdrop_url,''), COUNT(cm.media_id) FROM collections c LEFT JOIN collection_movies cm ON cm.collection_id=c.id GROUP BY c.id ORDER BY c.name`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		for rows.Next() {
			var id, tmdbID, count int
			var name, poster, backdrop string
			if rows.Scan(&id, &tmdbID, &name, &poster, &backdrop, &count) == nil {
				items = append(items, fiber.Map{"id": id, "tmdb_id": tmdbID, "name": name, "poster_url": poster, "backdrop_url": backdrop, "movie_count": count})
			}
		}
		return c.JSON(fiber.Map{"collections": items})
	})
	app.Get("/collections/:id", func(c fiber.Ctx) error {
		id, _ := strconv.Atoi(c.Params("id"))
		return c.JSON(fiber.Map{"movies": mediaList(`SELECT `+mediaCols+` FROM media m JOIN collection_movies cm ON cm.media_id=m.id WHERE cm.collection_id=$1 ORDER BY m.year ASC, m.title ASC`, id)})
	})

	// Похожие фильмы (TMDb recommendations, сопоставление с библиотекой)
	app.Get("/movies/:id/similar", func(c fiber.Ctx) error {
		id, _ := strconv.Atoi(c.Params("id"))
		var tmdbID int
		if err := db.QueryRow(ctx, `SELECT tmdb_id FROM media WHERE id=$1`, id).Scan(&tmdbID); err != nil || tmdbID <= 0 {
			return c.JSON(fiber.Map{"items": []mediaItem{}})
		}
		key := platform.GetEnv("TMDB_API_KEY", "")
		lang := platform.GetEnv("TMDB_LANG", "ru-RU")
		u := fmt.Sprintf("https://api.themoviedb.org/3/movie/%d/recommendations?api_key=%s&language=%s", tmdbID, key, lang)
		var res struct {
			Results []struct {
				ID int `json:"id"`
			} `json:"results"`
		}
		if err := httpGetJSON(u, &res); err != nil || len(res.Results) == 0 {
			return c.JSON(fiber.Map{"items": []mediaItem{}})
		}
		placeholders := make([]string, 0, len(res.Results))
		args := []interface{}{}
		for i, r := range res.Results {
			placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
			args = append(args, r.ID)
		}
		query := `SELECT ` + mediaCols + ` FROM media WHERE tmdb_id IN (` + strings.Join(placeholders, ",") + `) AND media_type='movie' LIMIT 20`
		return c.JSON(fiber.Map{"items": mediaList(query, args...)})
	})

	// Персона: имя, аватар, фильмы/сериалы с её участием
	app.Get("/people/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var name, profile string
		if err := db.QueryRow(ctx, `SELECT name, COALESCE(profile_url,'') FROM people WHERE id=$1`, id).Scan(&name, &profile); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		rows, err := db.Query(ctx, `
			SELECT `+mediaCols+` FROM media m
			JOIN media_people mp ON mp.media_id = m.id
			WHERE mp.person_id=$1
			ORDER BY m.year DESC, m.title ASC`, id)
		items := []mediaItem{}
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var m mediaItem
				if rows.Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID, &m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime) == nil {
					items = append(items, m)
				}
			}
		}
		return c.JSON(fiber.Map{"id": id, "name": name, "profile_url": profile, "movies": items})
	})

	// Предпросмотр авто-переименования
	app.Post("/rename/preview", func(c fiber.Ctx) error {
		var body struct {
			ID int `json:"id"`
		}
		if err := c.Bind().JSON(&body); err != nil || body.ID <= 0 {
			return c.Status(400).JSON(fiber.Map{"error": "нужен id"})
		}
		p, err := buildRenamePreview(body.ID)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(p)
	})

	// Применить авто-переименование
	app.Post("/rename/apply", func(c fiber.Ctx) error {
		var body struct {
			ID int `json:"id"`
		}
		if err := c.Bind().JSON(&body); err != nil || body.ID <= 0 {
			return c.Status(400).JSON(fiber.Map{"error": "нужен id"})
		}
		if err := applyRename(body.ID); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "renamed"})
	})

	// Статистика библиотеки
	app.Get("/library/stats", func(c fiber.Ctx) error {
		var movieTotal, movieWithPoster, tvTotal, tvWithPoster, episodeTotal int
		_ = db.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE poster_url <> '') FROM media WHERE media_type='movie'`).Scan(&movieTotal, &movieWithPoster)
		_ = db.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE poster_url <> '') FROM media WHERE media_type='tv'`).Scan(&tvTotal, &tvWithPoster)
		_ = db.QueryRow(ctx, `SELECT COUNT(*) FROM episodes`).Scan(&episodeTotal)

		movieGenres := map[string]int{}
		tvGenres := map[string]int{}
		rows, _ := db.Query(ctx, `SELECT media_type, genres FROM media WHERE genres <> ''`)
		for rows.Next() {
			var mt, g string
			if rows.Scan(&mt, &g) == nil {
				for _, part := range strings.Split(g, ",") {
					p := strings.TrimSpace(part)
					if p == "" {
						continue
					}
					if mt == "movie" {
						movieGenres[p]++
					} else {
						tvGenres[p]++
					}
				}
			}
		}
		rows.Close()

		movieYears := []fiber.Map{}
		yrows, _ := db.Query(ctx, `SELECT year, COUNT(*) FROM media WHERE media_type='movie' AND year > 0 GROUP BY year ORDER BY year`)
		for yrows.Next() {
			var y, c int
			if yrows.Scan(&y, &c) == nil {
				movieYears = append(movieYears, fiber.Map{"year": y, "count": c})
			}
		}
		yrows.Close()

		return c.JSON(fiber.Map{
			"movies": fiber.Map{"total": movieTotal, "with_poster": movieWithPoster, "by_genre": movieGenres, "by_year": movieYears},
			"tv":     fiber.Map{"total": tvTotal, "episodes": episodeTotal, "with_poster": tvWithPoster, "by_genre": tvGenres},
			"top":    fiber.Map{"movies": topRated("movie", 10), "tv": topRated("tv", 10)},
		})
	})

	// ============================================================
	// TMDb: тренды / новинки / топы (для Киноафиши)
	// ============================================================
	app.Get("/tmdb/genres", func(c fiber.Ctx) error {
		mediaType := c.Query("media_type", "movie")
		if mediaType != "tv" {
			mediaType = "movie"
		}
		return c.JSON(fiber.Map{"genres": tmdbGenres(mediaType)})
	})

	app.Get("/tmdb/trailer", func(c fiber.Ctx) error {
		mediaType := c.Query("media_type", "movie")
		if mediaType != "tv" {
			mediaType = "movie"
		}
		id, _ := strconv.Atoi(c.Query("tmdb_id"))
		if id <= 0 {
			return c.Status(400).JSON(fiber.Map{"error": "tmdb_id required"})
		}
		return c.JSON(fiber.Map{"trailer_url": tmdbTrailer(mediaType, id)})
	})

	app.Get("/tmdb/:category", func(c fiber.Ctx) error {
		category := c.Params("category")
		if _, ok := tmdbCategories[category]; !ok {
			return c.Status(404).JSON(fiber.Map{"error": "unknown category"})
		}
		page, _ := strconv.Atoi(c.Query("page", "1"))
		if page < 1 {
			page = 1
		}
		items, totalPages, err := tmdbList(category, page)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{
			"category":    category,
			"page":        page,
			"total_pages": totalPages,
			"items":       items,
		})
	})

	// ============================================================
	// WISHLIST
	// ============================================================
	app.Get("/wishlist", func(c fiber.Ctx) error {
		items := wishlistList()
		return c.JSON(fiber.Map{"items": items, "count": len(items)})
	})

	app.Post("/wishlist", func(c fiber.Ctx) error {
		var body struct {
			TmdbID    int    `json:"tmdb_id"`
			MediaType string `json:"media_type"`
		}
		if err := c.Bind().JSON(&body); err != nil || body.TmdbID <= 0 {
			return c.Status(400).JSON(fiber.Map{"error": "нужны tmdb_id и media_type"})
		}
		if body.MediaType != "tv" {
			body.MediaType = "movie"
		}
		item, err := wishlistAdd(body.TmdbID, body.MediaType)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		_ = rdb.Publish(ctx, "wishlist.added", item.Title).Err()
		return c.JSON(item)
	})

	app.Delete("/wishlist/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		if _, err := db.Exec(ctx, `DELETE FROM wishlist WHERE id=$1`, id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "deleted", "id": id})
	})

	app.Get("/wishlist/check", func(c fiber.Ctx) error {
		released := wishlistCheck(0)
		return c.JSON(fiber.Map{"released": released, "count": len(released)})
	})

	app.Post("/wishlist/:id/check", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		released := wishlistCheck(id)
		return c.JSON(fiber.Map{"released": released, "count": len(released)})
	})

	app.Get("/wishlist/released/count", func(c fiber.Ctx) error {
		n, _ := rdb.Get(ctx, "wishlist:released_count").Int()
		return c.JSON(fiber.Map{"count": n})
	})

	app.Post("/wishlist/released/reset", func(c fiber.Ctx) error {
		_ = rdb.Set(ctx, "wishlist:released_count", "0", 0).Err()
		return c.JSON(fiber.Map{"status": "ok", "count": 0})
	})

	// ============================================================
	// ПЛЕЙЛИСТЫ ФИЛЬМОВ
	// ============================================================
	app.Get("/movie-playlists", func(c fiber.Ctx) error {
		rows, err := db.Query(ctx, `
			SELECT p.id, p.name, COALESCE(p.description,''), p.created_at,
			       (SELECT COUNT(*) FROM movie_playlist_items i WHERE i.playlist_id=p.id)
			FROM movie_playlists p ORDER BY p.created_at DESC`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		for rows.Next() {
			var id, count int
			var name, desc string
			var created time.Time
			if rows.Scan(&id, &name, &desc, &created, &count) == nil {
				items = append(items, fiber.Map{"id": id, "name": name, "description": desc, "count": count, "created_at": created})
			}
		}
		return c.JSON(fiber.Map{"playlists": items})
	})

	app.Post("/movie-playlists", func(c fiber.Ctx) error {
		var b struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := c.Bind().JSON(&b); err != nil || strings.TrimSpace(b.Name) == "" {
			return c.Status(400).JSON(fiber.Map{"error": "нужно имя"})
		}
		var id int
		err := db.QueryRow(ctx, `INSERT INTO movie_playlists (name, description) VALUES ($1,$2) RETURNING id`, b.Name, b.Description).Scan(&id)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"id": id, "name": b.Name, "description": b.Description, "count": 0})
	})

	app.Delete("/movie-playlists/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		_, _ = db.Exec(ctx, `DELETE FROM movie_playlists WHERE id=$1`, id)
		return c.JSON(fiber.Map{"status": "deleted", "id": id})
	})

	app.Get("/movie-playlists/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var name, desc string
		if err := db.QueryRow(ctx, `SELECT name, COALESCE(description,'') FROM movie_playlists WHERE id=$1`, id).Scan(&name, &desc); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "not found"})
		}
		rows, err := db.Query(ctx, `
			SELECT `+mediaCols+` FROM media m
			JOIN movie_playlist_items i ON i.media_id = m.id
			WHERE i.playlist_id=$1 ORDER BY COALESCE(i.ord,0), m.title`, id)
		movies := []mediaItem{}
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var m mediaItem
				if rows.Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID, &m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime) == nil {
					movies = append(movies, m)
				}
			}
		}
		return c.JSON(fiber.Map{"id": id, "name": name, "description": desc, "movies": movies})
	})

	app.Post("/movie-playlists/:id/add", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		var b struct {
			MediaID int `json:"media_id"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.MediaID <= 0 {
			return c.Status(400).JSON(fiber.Map{"error": "нужен media_id"})
		}
		_, err = db.Exec(ctx, `
			INSERT INTO movie_playlist_items (playlist_id, media_id, ord)
			VALUES ($1,$2,(SELECT COALESCE(MAX(ord),0)+1 FROM movie_playlist_items WHERE playlist_id=$1))
			ON CONFLICT DO NOTHING`, id, b.MediaID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "added"})
	})

	app.Delete("/movie-playlists/:id/items/:media_id", func(c fiber.Ctx) error {
		id, _ := strconv.Atoi(c.Params("id"))
		mid, _ := strconv.Atoi(c.Params("media_id"))
		_, _ = db.Exec(ctx, `DELETE FROM movie_playlist_items WHERE playlist_id=$1 AND media_id=$2`, id, mid)
		return c.JSON(fiber.Map{"status": "removed"})
	})

	// ============================================================
	// ПОДПИСКИ НА СЕРИАЛЫ
	// ============================================================
	app.Get("/subscriptions", func(c fiber.Ctx) error {
		items := subscriptionList()
		return c.JSON(fiber.Map{"items": items, "count": len(items)})
	})

	app.Post("/subscriptions", func(c fiber.Ctx) error {
		var b struct {
			SeriesID int `json:"series_id"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.SeriesID <= 0 {
			return c.Status(400).JSON(fiber.Map{"error": "нужен series_id"})
		}
		res, err := subscriptionAdd(b.SeriesID)
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(res)
	})

	app.Delete("/subscriptions/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		_, _ = db.Exec(ctx, `DELETE FROM series_subscriptions WHERE id=$1`, id)
		return c.JSON(fiber.Map{"status": "deleted", "id": id})
	})

	app.Post("/subscriptions/:id/check", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		added, err := subscriptionCheck(id)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "checked", "added": added, "count": len(added)})
	})

	log.Println("🎬 media-service на :8091")
	log.Fatal(app.Listen(":8091"))
}

// startAutoScan — фоновый периодический скан библиотеки.
func startAutoScan() {
	interval := 60
	if v, err := strconv.Atoi(platform.GetEnv("MEDIA_SCAN_INTERVAL_MINUTES", "60")); err == nil && v > 0 {
		interval = v
	}
	log.Printf("🔄 Авто-скан каждые %d минут", interval)
	for {
		time.Sleep(time.Duration(interval) * time.Minute)
		ok, err := rdb.SetNX(ctx, "media:scan:running", "true", 10*time.Minute).Result()
		if err != nil || !ok {
			continue
		}
		_ = rdb.Set(ctx, "media:scan:progress", "0", 10*time.Minute).Err()
		_ = rdb.Set(ctx, "media:scan:total", "0", 10*time.Minute).Err()
		go runScan()
	}
}

// ============================================================
// WATCHDOG (fsnotify) — мгновенное обнаружение новых файлов
// ============================================================

func isVideoExt(ext string) bool {
	switch ext {
	case ".mkv", ".mp4", ".avi", ".m4v", ".mov", ".webm":
		return true
	}
	return false
}

func isAudioExt(ext string) bool {
	switch ext {
	case ".flac", ".mp3", ".m4a", ".aac", ".ogg", ".opus", ".wav", ".dsf", ".dff", ".ape":
		return true
	}
	return false
}

func isPhotoExt(ext string) bool {
	return photoImageExts[ext]
}

func addRecursive(w *fsnotify.Watcher, root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if aerr := w.Add(path); aerr != nil {
				log.Printf("⚠️ watch add %s: %v", path, aerr)
			}
		}
		return nil
	})
}

var knownFiles = struct {
	sync.Mutex
	m map[string]int64
}{m: map[string]int64{}}

// watchDirs возвращает существующие каталоги для наблюдения.
func watchDirs() []string {
	dirs := []string{}
	for _, d := range []string{
		platform.GetEnv("MEDIA_MOVIES_DIR", "/movies"),
		platform.GetEnv("MEDIA_TV_DIR", "/tv"),
		platform.GetEnv("MEDIA_PHOTOS_DIR", "/photos"),
	} {
		if _, err := os.Stat(d); err == nil {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func isMediaFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return isVideoExt(ext) || isAudioExt(ext) || isPhotoExt(ext)
}

func knownMtime(path string) (int64, bool) {
	knownFiles.Lock()
	defer knownFiles.Unlock()
	v, ok := knownFiles.m[path]
	return v, ok
}

func setKnown(path string, mtime int64) {
	knownFiles.Lock()
	knownFiles.m[path] = mtime
	knownFiles.Unlock()
}

func seedKnownFiles(dirs []string) {
	n := 0
	for _, root := range dirs {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !isMediaFile(p) {
				return nil
			}
			setKnown(p, info.ModTime().Unix())
			n++
			return nil
		})
	}
	log.Printf("👀 Watchdog: базовый список %d файлов", n)
}

func startWatchdog() {
	if platform.GetEnv("MEDIA_WATCH_ENABLED", "true") != "true" {
		log.Println("👀 Watchdog отключён (MEDIA_WATCH_ENABLED != true)")
		return
	}
	debounceSec := 3
	if v, err := strconv.Atoi(platform.GetEnv("MEDIA_WATCH_DEBOUNCE_SEC", "3")); err == nil && v > 0 {
		debounceSec = v
	}
	roots := watchDirs()
	if len(roots) == 0 {
		log.Println("⚠️ Watchdog: нет доступных каталогов для наблюдения")
		return
	}
	seedKnownFiles(roots)
	debounceDur := time.Duration(debounceSec) * time.Second

	// fsnotify — мгновенно на локальных ФС (на сетевых CIFS/NFS событий нет)
	if w, err := fsnotify.NewWatcher(); err == nil {
		for _, d := range roots {
			addRecursive(w, d)
		}
		go fsnotifyLoop(w, debounceDur)
	} else {
		log.Printf("⚠️ fsnotify недоступен: %v", err)
	}

	// polling — надёжный fallback для сетевых ФС
	pollSec := 20
	if v, err := strconv.Atoi(platform.GetEnv("MEDIA_WATCH_POLL_SEC", "20")); err == nil && v > 0 {
		pollSec = v
	}
	go pollLoop(roots, time.Duration(pollSec)*time.Second, debounceDur)

	log.Printf("👀 Watchdog запущен (%d каталогов, fsnotify + polling %dс, debounce %dс)", len(roots), pollSec, debounceSec)
}

func fsnotifyLoop(w *fsnotify.Watcher, debounceDur time.Duration) {
	defer w.Close()
	debounce := map[string]time.Time{}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			if !isMediaFile(event.Name) {
				if event.Op&fsnotify.Create != 0 {
					if fi, serr := os.Stat(event.Name); serr == nil && fi.IsDir() {
						addRecursive(w, event.Name)
					}
				}
				continue
			}
			debounce[event.Name] = time.Now()

		case err := <-w.Errors:
			log.Printf("⚠️ watcher error: %v", err)

		case <-ticker.C:
			var ready []string
			for p, t := range debounce {
				if time.Since(t) > debounceDur {
					ready = append(ready, p)
					delete(debounce, p)
				}
			}
			for _, p := range ready {
				if fi, err := os.Stat(p); err == nil {
					setKnown(p, fi.ModTime().Unix())
				}
				handleNewFile(p)
			}
		}
	}
}

// pollLoop периодически сканирует каталоги и обрабатывает новые/изменённые файлы.
func pollLoop(roots []string, interval, debounceDur time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		for _, root := range roots {
			_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() || !isMediaFile(p) {
					return nil
				}
				mtime := info.ModTime().Unix()
				if prev, ok := knownMtime(p); ok && prev == mtime {
					return nil
				}
				// ждём, пока файл «устоится»
				if time.Since(info.ModTime()) < debounceDur {
					return nil
				}
				setKnown(p, mtime)
				handleNewFile(p)
				return nil
			})
		}
	}
}

func handleNewFile(path string) {
	ext := strings.ToLower(filepath.Ext(path))
	moviesDir := platform.GetEnv("MEDIA_MOVIES_DIR", "/movies")
	tvDir := platform.GetEnv("MEDIA_TV_DIR", "/tv")
	photosDir := platform.GetEnv("MEDIA_PHOTOS_DIR", "/photos")

	var kind, title string

	switch {
	case strings.HasPrefix(path, moviesDir+string(os.PathSeparator)) && isVideoExt(ext):
		kind = "movie"
		t, y := parseName(filepath.Base(path))
		title = titleWithYear(t, y)
		if _, _, err := upsertMedia(path, "movie", t, y); err != nil {
			log.Printf("⚠️ watchdog upsert movie %s: %v", path, err)
			return
		}

	case strings.HasPrefix(path, tvDir+string(os.PathSeparator)) && isVideoExt(ext):
		kind = "tv"
		rel := strings.TrimPrefix(path, tvDir+string(os.PathSeparator))
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) == 0 || parts[0] == "" {
			return
		}
		seriesDir := filepath.Join(tvDir, parts[0])
		if fi, err := os.Stat(seriesDir); err != nil || !fi.IsDir() {
			return
		}
		t, y := parseName(filepath.Base(seriesDir))
		title = titleWithYear(t, y)
		if _, _, err := upsertMedia(seriesDir, "tv", t, y); err != nil {
			log.Printf("⚠️ watchdog upsert tv %s: %v", seriesDir, err)
			return
		}
		if seriesID, err := mediaIDByPath(seriesDir); err == nil {
			scanEpisodes(seriesID, seriesDir)
		}

	case strings.HasPrefix(path, photosDir+string(os.PathSeparator)) && isPhotoExt(ext):
		kind = "photo"
		title = filepath.Base(path)

	default:
		return
	}

	// Уведомление в history через Redis
	msg := fmt.Sprintf("%s|%s", kind, filepath.Base(path))
	if err := rdb.Publish(ctx, "media.new_file", msg).Err(); err != nil {
		log.Printf("⚠️ watchdog publish: %v", err)
	}
	_ = rdb.Incr(ctx, "media:new_count").Err()
	log.Printf("🆕 Watchdog: %s [%s] %s", title, kind, filepath.Base(path))
}

func migrate() error {
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS media (
			id SERIAL PRIMARY KEY,
			path TEXT UNIQUE NOT NULL,
			media_type VARCHAR(16) NOT NULL,
			title TEXT NOT NULL,
			original_title TEXT,
			year INT,
			tmdb_id INT,
			overview TEXT,
			poster_url TEXT,
			backdrop_url TEXT,
			rating REAL,
			genres TEXT,
			runtime INT,
			added_at TIMESTAMP DEFAULT NOW(),
			scanned_at TIMESTAMP DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_media_type ON media(media_type);
		CREATE INDEX IF NOT EXISTS idx_media_title ON media(title);

		CREATE TABLE IF NOT EXISTS episodes (
			id SERIAL PRIMARY KEY,
			series_id INT NOT NULL REFERENCES media(id) ON DELETE CASCADE,
			season INT NOT NULL,
			episode INT NOT NULL,
			path TEXT UNIQUE NOT NULL,
			title TEXT,
			overview TEXT,
			runtime INT,
			air_date DATE,
			thumbnail_url TEXT,
			tmdb_episode_id INT,
			video_codec VARCHAR(32),
			audio_codec VARCHAR(32),
			scanned_at TIMESTAMP DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_episodes_series ON episodes(series_id);
		CREATE INDEX IF NOT EXISTS idx_episodes_season ON episodes(series_id, season, episode);

		CREATE TABLE IF NOT EXISTS playback_progress (
			id SERIAL PRIMARY KEY,
			media_type VARCHAR(16) NOT NULL,
			media_id INT NOT NULL,
			position_sec INT NOT NULL DEFAULT 0,
			duration_sec INT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(media_type, media_id)
		);

		CREATE TABLE IF NOT EXISTS collections (
			id SERIAL PRIMARY KEY,
			tmdb_id INT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			overview TEXT,
			poster_url TEXT,
			backdrop_url TEXT,
			scanned_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS collection_movies (
			collection_id INT REFERENCES collections(id) ON DELETE CASCADE,
			media_id INT REFERENCES media(id) ON DELETE CASCADE,
			PRIMARY KEY (collection_id, media_id)
		);

		CREATE TABLE IF NOT EXISTS people (
			id SERIAL PRIMARY KEY,
			tmdb_id INT UNIQUE NOT NULL,
			name TEXT NOT NULL,
			profile_url TEXT,
			scanned_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS media_people (
			media_id INT REFERENCES media(id) ON DELETE CASCADE,
			person_id INT REFERENCES people(id) ON DELETE CASCADE,
			role VARCHAR(32) NOT NULL,
			character TEXT,
			ord INT,
			PRIMARY KEY (media_id, person_id, role)
		);
		CREATE INDEX IF NOT EXISTS idx_media_people_media ON media_people(media_id);

		CREATE TABLE IF NOT EXISTS wishlist (
			id SERIAL PRIMARY KEY,
			tmdb_id INT NOT NULL,
			media_type VARCHAR(16) NOT NULL,
			title TEXT NOT NULL,
			year INT,
			poster_url TEXT,
			overview TEXT,
			rating REAL,
			genres TEXT,
			release_date DATE,
			added_at TIMESTAMPTZ DEFAULT NOW(),
			notified BOOLEAN DEFAULT FALSE,
			UNIQUE(tmdb_id, media_type)
		);
		CREATE INDEX IF NOT EXISTS idx_wishlist_type ON wishlist(media_type);

		CREATE TABLE IF NOT EXISTS movie_playlists (
			id SERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS movie_playlist_items (
			playlist_id INT REFERENCES movie_playlists(id) ON DELETE CASCADE,
			media_id INT REFERENCES media(id) ON DELETE CASCADE,
			ord INT,
			PRIMARY KEY (playlist_id, media_id)
		);

		CREATE TABLE IF NOT EXISTS series_subscriptions (
			id SERIAL PRIMARY KEY,
			series_id INT REFERENCES media(id) ON DELETE CASCADE,
			title TEXT NOT NULL,
			season INT,
			last_episode INT DEFAULT 0,
			last_check TIMESTAMPTZ DEFAULT NOW(),
			enabled BOOLEAN DEFAULT TRUE,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(series_id)
		);
	`)
	return err
}

type mediaFilter struct {
	Q         string
	Genre     string
	YearMin   int
	YearMax   int
	RatingMin float64
	Sort      string
	Order     string
	Limit     int
	Offset    int
}

func topRated(mediaType string, limit int) []mediaItem {
	rows, err := db.Query(ctx, `SELECT `+mediaCols+` FROM media WHERE media_type=$1 ORDER BY rating DESC NULLS LAST, title ASC LIMIT $2`, mediaType, limit)
	if err != nil {
		return []mediaItem{}
	}
	defer rows.Close()
	items := []mediaItem{}
	for rows.Next() {
		var m mediaItem
		if rows.Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID, &m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime) == nil {
			items = append(items, m)
		}
	}
	return items
}

func mediaList(query string, args ...interface{}) []mediaItem {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return []mediaItem{}
	}
	defer rows.Close()
	items := []mediaItem{}
	for rows.Next() {
		var m mediaItem
		if rows.Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID, &m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime) == nil {
			items = append(items, m)
		}
	}
	return items
}

func parseMediaFilter(c fiber.Ctx) mediaFilter {
	limit, _ := strconv.Atoi(c.Query("limit", "50"))
	offset, _ := strconv.Atoi(c.Query("offset", "0"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	ymin, _ := strconv.Atoi(c.Query("year_min"))
	ymax, _ := strconv.Atoi(c.Query("year_max"))
	rmin, _ := strconv.ParseFloat(c.Query("rating_min"), 64)
	return mediaFilter{
		Q:         c.Query("q"),
		Genre:     c.Query("genre"),
		YearMin:   ymin,
		YearMax:   ymax,
		RatingMin: rmin,
		Sort:      c.Query("sort"),
		Order:     c.Query("order"),
		Limit:     limit,
		Offset:    offset,
	}
}

func queryMedia(mediaType string, f mediaFilter) []mediaItem {
	query := `SELECT ` + mediaCols + ` FROM media WHERE media_type=$1`
	args := []interface{}{mediaType}
	arg := 2
	if f.Q != "" {
		query += fmt.Sprintf(` AND title ILIKE '%%' || $%d || '%%'`, arg)
		args = append(args, f.Q)
		arg++
	}
	if f.Genre != "" {
		query += fmt.Sprintf(` AND genres ILIKE '%%' || $%d || '%%'`, arg)
		args = append(args, f.Genre)
		arg++
	}
	if f.YearMin > 0 {
		query += fmt.Sprintf(` AND year >= $%d`, arg)
		args = append(args, f.YearMin)
		arg++
	}
	if f.YearMax > 0 {
		query += fmt.Sprintf(` AND year <= $%d`, arg)
		args = append(args, f.YearMax)
		arg++
	}
	if f.RatingMin > 0 {
		query += fmt.Sprintf(` AND rating >= $%d`, arg)
		args = append(args, f.RatingMin)
		arg++
	}

	order := "ASC"
	if strings.ToLower(f.Order) == "desc" {
		order = "DESC"
	}
	switch f.Sort {
	case "year":
		query += " ORDER BY year " + order + ", title ASC"
	case "rating":
		query += " ORDER BY rating " + order + " NULLS LAST, title ASC"
	case "added":
		query += " ORDER BY added_at " + order
	default:
		query += " ORDER BY title " + order
	}
	query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, arg, arg+1)
	args = append(args, f.Limit, f.Offset)

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return []mediaItem{}
	}
	defer rows.Close()

	items := []mediaItem{}
	for rows.Next() {
		var m mediaItem
		if err := rows.Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID, &m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime); err != nil {
			continue
		}
		items = append(items, m)
	}
	return items
}

func mediaDetail(c fiber.Ctx) error {
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
	}
	var m mediaItem
	err = db.QueryRow(ctx, `SELECT `+mediaCols+` FROM media WHERE id=$1`, id).
		Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID, &m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "not found"})
	}
	if filePath, _, err := resolveMediaFile(id); err == nil {
		m.AudioCodec = audioCodec(filePath)
		m.VideoCodec = videoCodec(filePath)
	}
	if m.TMDBID > 0 {
		m.TrailerURL = tmdbTrailer(m.MediaType, m.TMDBID)
	}
	m.Actors, m.Director = loadPeople(id)
	return c.JSON(m)
}

// loadPeople возвращает актёров (по ord) и первого режиссёра для медиа.
func loadPeople(mediaID int) ([]personRef, *personRef) {
	actors := []personRef{}
	var director *personRef

	rows, err := db.Query(ctx, `
		SELECT p.id, p.name, COALESCE(p.profile_url,''), COALESCE(mp.character,''), mp.role
		FROM media_people mp JOIN people p ON p.id = mp.person_id
		WHERE mp.media_id=$1
		ORDER BY CASE WHEN mp.role='actor' THEN 0 ELSE 1 END, COALESCE(mp.ord, 0)`, mediaID)
	if err != nil {
		return actors, director
	}
	defer rows.Close()
	for rows.Next() {
		var pr personRef
		var role string
		if rows.Scan(&pr.ID, &pr.Name, &pr.ProfileURL, &pr.Character, &role) != nil {
			continue
		}
		if role == "director" {
			if director == nil {
				d := pr
				director = &d
			}
			continue
		}
		actors = append(actors, pr)
	}
	return actors, director
}

// ============================================================
// АВТО-ПЕРЕИМЕНОВАНИЕ
// ============================================================

type renameEpisode struct {
	ID            int    `json:"id"`
	Season        int    `json:"season"`
	Episode       int    `json:"episode"`
	CurrentPath   string `json:"current_path"`
	SuggestedPath string `json:"suggested_path"`
}

type renamePreview struct {
	ID            int             `json:"id"`
	MediaType     string          `json:"media_type"`
	CurrentPath   string          `json:"current_path"`
	SuggestedPath string          `json:"suggested_path"`
	Exists        bool            `json:"exists"`
	Episodes      []renameEpisode `json:"episodes,omitempty"`
}

var invalidNameChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func sanitizeName(s string) string {
	s = invalidNameChars.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ".")
	if s == "" {
		s = "Без названия"
	}
	return s
}

func titleWithYear(title string, year int) string {
	title = sanitizeName(title)
	if year > 0 {
		return fmt.Sprintf("%s (%d)", title, year)
	}
	return title
}

func buildRenamePreview(id int) (*renamePreview, error) {
	var m mediaItem
	err := db.QueryRow(ctx, `SELECT `+mediaCols+`, path FROM media WHERE id=$1`, id).
		Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID, &m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime, &m.Path)
	if err != nil {
		return nil, fmt.Errorf("не найдено")
	}
	base := m.Title
	if base == "" {
		base = m.OriginalTitle
	}
	proper := titleWithYear(base, m.Year)

	p := &renamePreview{
		ID:          id,
		MediaType:   m.MediaType,
		CurrentPath: m.Path,
	}

	if m.MediaType == "tv" {
		p.SuggestedPath = proper
		rows, qerr := db.Query(ctx, `
			SELECT id, season, episode, COALESCE(title,''), path
			FROM episodes WHERE series_id=$1 ORDER BY season, episode`, id)
		if qerr == nil {
			defer rows.Close()
			for rows.Next() {
				var ep renameEpisode
				var epTitle string
				if rows.Scan(&ep.ID, &ep.Season, &ep.Episode, &epTitle, &ep.CurrentPath) != nil {
					continue
				}
				ext := filepath.Ext(ep.CurrentPath)
				if epTitle == "" {
					epTitle = fmt.Sprintf("Эпизод %d", ep.Episode)
				}
				fname := fmt.Sprintf("%s - S%02dE%02d - %s%s", sanitizeName(base), ep.Season, ep.Episode, sanitizeName(epTitle), ext)
				ep.SuggestedPath = filepath.Join(proper, fmt.Sprintf("Season %02d", ep.Season), fname)
				p.Episodes = append(p.Episodes, ep)
			}
		}
	} else {
		ext := filepath.Ext(m.Path)
		p.SuggestedPath = filepath.Join(proper, proper+ext)
	}

	// проверяем, существует ли уже целевой путь
	var targetAbs string
	if m.MediaType == "tv" {
		targetAbs = filepath.Join(platform.GetEnv("MEDIA_TV_DIR", "/tv"), p.SuggestedPath)
	} else {
		targetAbs = filepath.Join(platform.GetEnv("MEDIA_MOVIES_DIR", "/movies"), p.SuggestedPath)
	}
	if _, statErr := os.Stat(targetAbs); statErr == nil {
		p.Exists = true
	}
	return p, nil
}

func applyRename(id int) error {
	p, err := buildRenamePreview(id)
	if err != nil {
		return err
	}
	if p.MediaType == "tv" {
		tvDir := platform.GetEnv("MEDIA_TV_DIR", "/tv")
		oldDir := p.CurrentPath
		newDir := filepath.Join(tvDir, p.SuggestedPath)
		if oldDir == newDir {
			return nil
		}
		if _, err := os.Stat(newDir); err == nil {
			return fmt.Errorf("целевой путь уже существует")
		}
		if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
			return err
		}
		if err := os.Rename(oldDir, newDir); err != nil {
			return fmt.Errorf("os.Rename: %w", err)
		}
		_, _ = db.Exec(ctx, `UPDATE media SET path=$1 WHERE id=$2`, newDir, id)
		// обновляем пути эпизодов с новым префиксом
		for _, ep := range p.Episodes {
			rel, rerr := filepath.Rel(oldDir, ep.CurrentPath)
			if rerr != nil {
				continue
			}
			_, _ = db.Exec(ctx, `UPDATE episodes SET path=$1 WHERE id=$2`, filepath.Join(newDir, rel), ep.ID)
		}
		return nil
	}

	moviesDir := platform.GetEnv("MEDIA_MOVIES_DIR", "/movies")
	newPath := filepath.Join(moviesDir, p.SuggestedPath)
	if p.CurrentPath == newPath {
		return nil
	}
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("целевой путь уже существует")
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return err
	}
	if err := os.Rename(p.CurrentPath, newPath); err != nil {
		return fmt.Errorf("os.Rename: %w", err)
	}
	_, _ = db.Exec(ctx, `UPDATE media SET path=$1 WHERE id=$2`, newPath, id)
	return nil
}

// ============================================================
// СКАНИРОВАНИЕ
// ============================================================

type scanItem struct {
	path      string
	mediaType string
}

func runScan() {
	moviesDir := platform.GetEnv("MEDIA_MOVIES_DIR", "/movies")
	tvDir := platform.GetEnv("MEDIA_TV_DIR", "/tv")
	videoExts := map[string]bool{".mkv": true, ".mp4": true, ".avi": true}

	items := []scanItem{}
	filepath.WalkDir(moviesDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !videoExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		items = append(items, scanItem{path: p, mediaType: "movie"})
		return nil
	})
	if entries, err := os.ReadDir(tvDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				items = append(items, scanItem{path: filepath.Join(tvDir, e.Name()), mediaType: "tv"})
			}
		}
	}

	total := len(items)
	_ = rdb.Set(ctx, "media:scan:total", strconv.Itoa(total), 10*time.Minute).Err()
	_ = rdb.Set(ctx, "media:scan:progress", "0", 10*time.Minute).Err()

	added, updated, errCount := 0, 0, 0
	for i, it := range items {
		title, year := parseName(filepath.Base(it.path))
		if title != "" {
			a, u, e := upsertMedia(it.path, it.mediaType, title, year)
			if e != nil {
				log.Printf("⚠️ %s: %v", filepath.Base(it.path), e)
				errCount++
			} else {
				added += a
				updated += u
				if it.mediaType == "tv" {
					if seriesID, err := mediaIDByPath(it.path); err == nil {
						scanEpisodes(seriesID, it.path)
					}
				}
			}
		}
		_ = rdb.Set(ctx, "media:scan:progress", strconv.Itoa(i+1), 10*time.Minute).Err()
	}

	_ = rdb.Set(ctx, "media:scan:running", "false", 10*time.Minute).Err()
	_ = rdb.Set(ctx, "media:scan:last", time.Now().Format(time.RFC3339), 10*time.Minute).Err()
	result, _ := json.Marshal(map[string]int{"added": added, "updated": updated, "errors": errCount, "total": total})
	_ = rdb.Set(ctx, "media:scan:result", string(result), 10*time.Minute).Err()
}

func upsertMedia(p, mediaType, title string, year int) (int, int, error) {
	var existingID int
	err := db.QueryRow(ctx, `SELECT id FROM media WHERE path = $1`, p).Scan(&existingID)
	exists := err == nil && existingID > 0

	d, err := lookupTMDB(mediaType, title, year)
	if err != nil {
		log.Printf("⚠️ TMDb: %v", err)
		d = nil
	}

	var tmdbID, runtime, finalYear int
	var originalTitle, overview, posterURL, backdropURL, genres string
	var rating float64

	if d != nil {
		tmdbID = d.ID
		originalTitle = d.OriginalTitle
		if originalTitle == "" {
			originalTitle = d.OriginalName
		}
		overview = d.Overview
		if d.PosterPath != "" {
			posterURL = "https://image.tmdb.org/t/p/w500" + d.PosterPath
		}
		if d.BackdropPath != "" {
			backdropURL = "https://image.tmdb.org/t/p/w1280" + d.BackdropPath
		}
		rating = d.VoteAverage
		names := []string{}
		for _, g := range d.Genres {
			names = append(names, g.Name)
		}
		genres = strings.Join(names, ", ")
		if mediaType == "tv" && len(d.EpisodeRunTime) > 0 {
			runtime = d.EpisodeRunTime[0]
		} else {
			runtime = d.Runtime
		}
	}

	finalYear = year
	if d != nil {
		dateStr := d.ReleaseDate
		if dateStr == "" {
			dateStr = d.FirstAirDate
		}
		if len(dateStr) >= 4 {
			if yy, e := strconv.Atoi(dateStr[:4]); e == nil {
				finalYear = yy
			}
		}
	}

	displayTitle := title
	if d != nil {
		if d.Title != "" {
			displayTitle = d.Title
		} else if d.Name != "" {
			displayTitle = d.Name
		}
	}

	if exists {
		_, err = db.Exec(ctx, `
			UPDATE media SET title=$1, original_title=$2, year=$3, tmdb_id=$4, overview=$5,
				poster_url=$6, backdrop_url=$7, rating=$8, genres=$9, runtime=$10, scanned_at=NOW()
			WHERE id=$11`,
			displayTitle, originalTitle, finalYear, tmdbID, overview, posterURL, backdropURL, rating, genres, runtime, existingID)
		if err != nil {
			return 0, 0, err
		}
		if mediaType == "movie" && d != nil {
			linkCollection(existingID, d)
		}
		if tmdbID > 0 && !hasPeople(existingID) {
			syncCredits(existingID, mediaType, tmdbID)
		}
		return 0, 1, nil
	}

	_, err = db.Exec(ctx, `
		INSERT INTO media (path, media_type, title, original_title, year, tmdb_id, overview, poster_url, backdrop_url, rating, genres, runtime)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		p, mediaType, displayTitle, originalTitle, finalYear, tmdbID, overview, posterURL, backdropURL, rating, genres, runtime)
	if err != nil {
		return 0, 0, err
	}
	var newID int
	_ = db.QueryRow(ctx, `SELECT id FROM media WHERE path=$1`, p).Scan(&newID)
	if mediaType == "movie" && d != nil {
		linkCollection(newID, d)
	}
	if tmdbID > 0 {
		syncCredits(newID, mediaType, tmdbID)
	}
	return 1, 0, nil
}

func hasPeople(mediaID int) bool {
	var n int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM media_people WHERE media_id=$1`, mediaID).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func linkCollection(mediaID int, d *tmdbDetail) {
	if d.BelongsToCollection == nil || d.BelongsToCollection.ID == 0 {
		return
	}
	poster := ""
	if d.BelongsToCollection.PosterPath != "" {
		poster = "https://image.tmdb.org/t/p/w500" + d.BelongsToCollection.PosterPath
	}
	backdrop := ""
	if d.BelongsToCollection.BackdropPath != "" {
		backdrop = "https://image.tmdb.org/t/p/w1280" + d.BelongsToCollection.BackdropPath
	}
	_, err := db.Exec(ctx, `
		INSERT INTO collections (tmdb_id, name, poster_url, backdrop_url, scanned_at)
		VALUES ($1,$2,$3,$4,NOW())
		ON CONFLICT (tmdb_id) DO UPDATE SET name=EXCLUDED.name, poster_url=EXCLUDED.poster_url, backdrop_url=EXCLUDED.backdrop_url, scanned_at=NOW()`,
		d.BelongsToCollection.ID, d.BelongsToCollection.Name, poster, backdrop)
	if err != nil {
		return
	}
	var colID int
	if err := db.QueryRow(ctx, `SELECT id FROM collections WHERE tmdb_id=$1`, d.BelongsToCollection.ID).Scan(&colID); err == nil {
		_, _ = db.Exec(ctx, `INSERT INTO collection_movies (collection_id, media_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, colID, mediaID)
	}
}

// ============================================================
// ЭПИЗОДЫ
// ============================================================

type episodeItem struct {
	ID           int    `json:"id"`
	SeriesID     int    `json:"series_id"`
	Season       int    `json:"season"`
	Episode      int    `json:"episode"`
	Title        string `json:"title"`
	Overview     string `json:"overview"`
	Runtime      int    `json:"runtime"`
	AirDate      string `json:"air_date,omitempty"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	VideoCodec   string `json:"video_codec,omitempty"`
	AudioCodec   string `json:"audio_codec,omitempty"`
	Path         string `json:"path,omitempty"`
}

const episodeCols = `id, series_id, season, episode, COALESCE(title,''), COALESCE(overview,''), COALESCE(runtime,0), COALESCE(air_date::text,''), COALESCE(thumbnail_url,''), COALESCE(video_codec,''), COALESCE(audio_codec,''), path`

func mediaIDByPath(path string) (int, error) {
	var id int
	err := db.QueryRow(ctx, `SELECT id FROM media WHERE path=$1`, path).Scan(&id)
	return id, err
}

func scanEpisodes(seriesID int, dir string) {
	videoExts := map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".m4v": true}
	files := []string{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !videoExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		files = append(files, p)
		return nil
	})
	sort.Strings(files)

	// сезон из имени папки (S07, S 07, сезон 7)
	seasonFromDir := 0
	if m := seasonDir.FindStringSubmatch(filepath.Base(dir)); m != nil {
		seasonFromDir, _ = strconv.Atoi(m[1])
	}

	for i, p := range files {
		season, episode, title := parseEpisode(filepath.Base(p))
		if season == 0 && episode == 0 {
			// fallback: нумеруем по порядку в папке
			season = seasonFromDir
			if season == 0 {
				season = 1
			}
			episode = i + 1
			title = ""
		} else if season == 0 {
			// паттерн 9 дал только эпизод — сезон из папки
			season = seasonFromDir
			if season == 0 {
				season = 1
			}
		}
		upsertEpisode(seriesID, p, season, episode, title, videoCodec(p), audioCodec(p))
	}
	enrichEpisodes(seriesID)
}

func upsertEpisode(seriesID int, path string, season, episode int, title, vc, ac string) {
	_, err := db.Exec(ctx, `
		INSERT INTO episodes (series_id, season, episode, path, title, video_codec, audio_codec, scanned_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())
		ON CONFLICT (path) DO UPDATE SET
			season=EXCLUDED.season, episode=EXCLUDED.episode, title=EXCLUDED.title,
			video_codec=EXCLUDED.video_codec, audio_codec=EXCLUDED.audio_codec, scanned_at=NOW()`,
		seriesID, season, episode, path, title, vc, ac)
	if err != nil {
		log.Printf("⚠️ эпизод %s: %v", filepath.Base(path), err)
	}
}

type tmdbSeason struct {
	Episodes []struct {
		EpisodeNumber int    `json:"episode_number"`
		Name          string `json:"name"`
		Overview      string `json:"overview"`
		Runtime       int    `json:"runtime"`
		AirDate       string `json:"air_date"`
		StillPath     string `json:"still_path"`
	} `json:"episodes"`
}

func enrichEpisodes(seriesID int) {
	var tmdbID int
	if err := db.QueryRow(ctx, `SELECT tmdb_id FROM media WHERE id=$1`, seriesID).Scan(&tmdbID); err != nil || tmdbID <= 0 {
		return
	}
	rows, err := db.Query(ctx, `SELECT DISTINCT season FROM episodes WHERE series_id=$1`, seriesID)
	if err != nil {
		return
	}
	defer rows.Close()

	seasons := []int{}
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err == nil {
			seasons = append(seasons, s)
		}
	}

	key := platform.GetEnv("TMDB_API_KEY", "")
	lang := platform.GetEnv("TMDB_LANG", "ru-RU")
	for _, season := range seasons {
		u := fmt.Sprintf("https://api.themoviedb.org/3/tv/%d/season/%d?api_key=%s&language=%s", tmdbID, season, key, lang)
		var ts tmdbSeason
		if err := httpGetJSON(u, &ts); err != nil {
			continue
		}
		for _, ep := range ts.Episodes {
			if ep.EpisodeNumber <= 0 {
				continue
			}
			var thumb string
			if ep.StillPath != "" {
				thumb = "https://image.tmdb.org/t/p/w300" + ep.StillPath
			}
			var airDate interface{} = nil
			if ep.AirDate != "" {
				airDate = ep.AirDate
			}
			_, _ = db.Exec(ctx, `
				UPDATE episodes SET
					title=COALESCE(NULLIF($1,''), title), overview=$2, runtime=$3,
					air_date=$4, thumbnail_url=$5, tmdb_episode_id=$6
				WHERE series_id=$7 AND season=$8 AND episode=$9`,
				ep.Name, ep.Overview, ep.Runtime, airDate, thumb, ep.EpisodeNumber, seriesID, season, ep.EpisodeNumber)
		}
	}
}

func listEpisodes(seriesID, season int) []episodeItem {
	rows, err := db.Query(ctx, `SELECT `+episodeCols+` FROM episodes WHERE series_id=$1 AND season=$2 ORDER BY episode ASC`, seriesID, season)
	if err != nil {
		return []episodeItem{}
	}
	defer rows.Close()
	items := []episodeItem{}
	for rows.Next() {
		var e episodeItem
		if rows.Scan(&e.ID, &e.SeriesID, &e.Season, &e.Episode, &e.Title, &e.Overview, &e.Runtime, &e.AirDate, &e.ThumbnailURL, &e.VideoCodec, &e.AudioCodec, &e.Path) == nil {
			items = append(items, e)
		}
	}
	return items
}

type subEntry struct {
	idx    int
	lang   string
	label  string
	forced bool
	path   string
}

func parseSubtitleLang(name string) (string, bool) {
	lower := strings.ToLower(name)
	forced := strings.Contains(lower, "forced")
	switch {
	case strings.Contains(lower, "ru"), strings.Contains(lower, "rus"):
		return "ru", forced
	case strings.Contains(lower, "en"), strings.Contains(lower, "eng"):
		return "en", forced
	default:
		return "xx", forced
	}
}

func subtitleLabel(lang string, forced bool, name string) string {
	label := name
	switch lang {
	case "ru":
		label = "Русские"
	case "en":
		label = "English"
	}
	if forced {
		label += " (forced)"
	}
	return label
}

func collectSubtitles(videoPath string) []subEntry {
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))
	entries, _ := os.ReadDir(dir)
	subs := []subEntry{}
	idx := 0
	add := func(name string) {
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".srt" && ext != ".ass" && ext != ".sub" {
			return
		}
		lang, forced := parseSubtitleLang(name)
		subs = append(subs, subEntry{idx: idx, lang: lang, label: subtitleLabel(lang, forced, name), forced: forced, path: filepath.Join(dir, name)})
		idx++
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), base) {
			add(e.Name())
		}
	}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), base) {
			add(e.Name())
		}
	}
	return subs
}

func findSubtitle(videoPath string) (string, error) {
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext == ".srt" || ext == ".vtt" || ext == ".ass" {
			if strings.HasPrefix(name, base) {
				return filepath.Join(dir, name), nil
			}
		}
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.ToLower(filepath.Ext(e.Name())) == ".srt" {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", fmt.Errorf("нет субтитров")
}

func episodeThumbnail(id int, videoPath string) string {
	thumbPath := fmt.Sprintf("/tmp/thumb-%d.jpg", id)
	if info, err := os.Stat(thumbPath); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		return thumbPath
	}
	_ = exec.Command("ffmpeg", "-y", "-ss", "00:05:00", "-i", videoPath, "-vframes", "1", "-vf", "scale=320:-2", thumbPath).Run()
	if _, err := os.Stat(thumbPath); err != nil {
		_ = exec.Command("ffmpeg", "-y", "-ss", "00:01:00", "-i", videoPath, "-vframes", "1", "-vf", "scale=320:-2", thumbPath).Run()
	}
	if _, err := os.Stat(thumbPath); err != nil {
		return ""
	}
	return thumbPath
}

// ============================================================
// TMDB
// ============================================================

type tmdbSearchResult struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	Name          string  `json:"name"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	VoteAverage   float64 `json:"vote_average"`
	PosterPath    string  `json:"poster_path"`
	Overview      string  `json:"overview"`
}

type tmdbSearchResponse struct {
	Results []tmdbSearchResult `json:"results"`
}

type tmdbDetail struct {
	ID             int     `json:"id"`
	Title          string  `json:"title"`
	Name           string  `json:"name"`
	OriginalTitle  string  `json:"original_title"`
	OriginalName   string  `json:"original_name"`
	ReleaseDate    string  `json:"release_date"`
	FirstAirDate   string  `json:"first_air_date"`
	VoteAverage    float64 `json:"vote_average"`
	PosterPath     string  `json:"poster_path"`
	BackdropPath   string  `json:"backdrop_path"`
	Overview       string  `json:"overview"`
	Runtime        int     `json:"runtime"`
	EpisodeRunTime []int   `json:"episode_run_time"`
	Genres         []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"genres"`
	BelongsToCollection *struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		PosterPath   string `json:"poster_path"`
		BackdropPath string `json:"backdrop_path"`
	} `json:"belongs_to_collection"`
}

func httpGetJSON(u string, out interface{}) error {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// tmdbGetJSON — как httpGetJSON, но с повтором при rate limit (429).
func tmdbGetJSON(u string, out interface{}) error {
	err := httpGetJSON(u, out)
	if err != nil && strings.Contains(err.Error(), "429") {
		time.Sleep(time.Second)
		err = httpGetJSON(u, out)
	}
	return err
}

// ============================================================
// TMDb LISTS (тренды/новинки) + WISHLIST
// ============================================================

type tmdbGenre struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type tmdbGenreList struct {
	Genres []tmdbGenre `json:"genres"`
}

var tmdbCategories = map[string]struct {
	Path      string
	MediaType string
}{
	"now_playing": {"/movie/now_playing", "movie"},
	"upcoming":    {"/movie/upcoming", "movie"},
	"popular":     {"/movie/popular", "movie"},
	"top_rated":   {"/movie/top_rated", "movie"},
	"tv_on_air":   {"/tv/on_the_air", "tv"},
}

type tmdbRawList struct {
	Page    int `json:"page"`
	Results []struct {
		ID            int     `json:"id"`
		Title         string  `json:"title"`
		Name          string  `json:"name"`
		OriginalTitle string  `json:"original_title"`
		OriginalName  string  `json:"original_name"`
		ReleaseDate   string  `json:"release_date"`
		FirstAirDate  string  `json:"first_air_date"`
		PosterPath    string  `json:"poster_path"`
		Overview      string  `json:"overview"`
		VoteAverage   float64 `json:"vote_average"`
		GenreIDs      []int   `json:"genre_ids"`
	} `json:"results"`
	TotalPages int `json:"total_pages"`
}

type tmdbListItem struct {
	TmdbID        int      `json:"tmdb_id"`
	Title         string   `json:"title"`
	OriginalTitle string   `json:"original_title"`
	Year          int      `json:"year"`
	PosterURL     string   `json:"poster_url"`
	Overview      string   `json:"overview"`
	Rating        float64  `json:"rating"`
	Genres        []string `json:"genres"`
	MediaType     string   `json:"media_type"`
	ReleaseDate   string   `json:"release_date"`
	InLibrary     bool     `json:"in_library"`
	MediaID       *int     `json:"media_id"`
}

func tmdbGenres(mediaType string) []tmdbGenre {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	cacheKey := "tmdb:genres:" + mediaType
	if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		var out []tmdbGenre
		if json.Unmarshal([]byte(cached), &out) == nil && len(out) > 0 {
			return out
		}
	}
	key := platform.GetEnv("TMDB_API_KEY", "")
	lang := platform.GetEnv("TMDB_LANG", "ru-RU")
	u := fmt.Sprintf("https://api.themoviedb.org/3/genre/%s/list?api_key=%s&language=%s", mediaType, key, lang)
	var gl tmdbGenreList
	out := []tmdbGenre{}
	if err := tmdbGetJSON(u, &gl); err == nil {
		out = gl.Genres
		if data, err := json.Marshal(out); err == nil {
			_ = rdb.Set(ctx, cacheKey, string(data), 24*time.Hour).Err()
		}
	}
	return out
}

func tmdbGenreMap(mediaType string) map[int]string {
	m := map[int]string{}
	for _, g := range tmdbGenres(mediaType) {
		m[g.ID] = g.Name
	}
	return m
}

// mediaIDsByTmdb — карта tmdb_id → media.id для проверки наличия в библиотеке.
func mediaIDsByTmdb(ids []int) map[int]int {
	res := map[int]int{}
	if len(ids) == 0 {
		return res
	}
	rows, err := db.Query(ctx, `SELECT tmdb_id, id FROM media WHERE tmdb_id = ANY($1)`, ids)
	if err != nil {
		return res
	}
	defer rows.Close()
	for rows.Next() {
		var tmdb, mid int
		if rows.Scan(&tmdb, &mid) == nil {
			res[tmdb] = mid
		}
	}
	return res
}

func tmdbList(category string, page int) ([]tmdbListItem, int, error) {
	cfg, ok := tmdbCategories[category]
	if !ok {
		return nil, 0, fmt.Errorf("unknown category")
	}
	cacheKey := fmt.Sprintf("tmdb:list:%s:%d", category, page)
	var raw tmdbRawList
	if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		_ = json.Unmarshal([]byte(cached), &raw)
	}
	if len(raw.Results) == 0 {
		key := platform.GetEnv("TMDB_API_KEY", "")
		lang := platform.GetEnv("TMDB_LANG", "ru-RU")
		u := fmt.Sprintf("https://api.themoviedb.org/3%s?api_key=%s&language=%s&page=%d", cfg.Path, key, lang, page)
		if err := tmdbGetJSON(u, &raw); err != nil {
			return nil, 0, err
		}
		if data, err := json.Marshal(raw); err == nil {
			_ = rdb.Set(ctx, cacheKey, string(data), time.Hour).Err()
		}
	}

	gmap := tmdbGenreMap(cfg.MediaType)
	ids := make([]int, 0, len(raw.Results))
	for _, r := range raw.Results {
		ids = append(ids, r.ID)
	}
	inLib := mediaIDsByTmdb(ids)

	items := make([]tmdbListItem, 0, len(raw.Results))
	for _, r := range raw.Results {
		title := r.Title
		if title == "" {
			title = r.Name
		}
		orig := r.OriginalTitle
		if orig == "" {
			orig = r.OriginalName
		}
		date := r.ReleaseDate
		if date == "" {
			date = r.FirstAirDate
		}
		year := 0
		if len(date) >= 4 {
			year, _ = strconv.Atoi(date[:4])
		}
		genres := []string{}
		for _, gid := range r.GenreIDs {
			if name, ok := gmap[gid]; ok {
				genres = append(genres, name)
			}
		}
		poster := ""
		if r.PosterPath != "" {
			poster = "https://image.tmdb.org/t/p/w500" + r.PosterPath
		}
		it := tmdbListItem{
			TmdbID: r.ID, Title: title, OriginalTitle: orig, Year: year,
			PosterURL: poster, Overview: r.Overview, Rating: r.VoteAverage,
			Genres: genres, MediaType: cfg.MediaType, ReleaseDate: date,
		}
		if mid, ok := inLib[r.ID]; ok {
			it.InLibrary = true
			m := mid
			it.MediaID = &m
		}
		items = append(items, it)
	}
	return items, raw.TotalPages, nil
}

// ---- Wishlist ----

type wishlistItem struct {
	ID          int       `json:"id"`
	TmdbID      int       `json:"tmdb_id"`
	MediaType   string    `json:"media_type"`
	Title       string    `json:"title"`
	Year        int       `json:"year"`
	PosterURL   string    `json:"poster_url"`
	Overview    string    `json:"overview"`
	Rating      float64   `json:"rating"`
	Genres      string    `json:"genres"`
	ReleaseDate string    `json:"release_date,omitempty"`
	AddedAt     time.Time `json:"added_at"`
	Notified    bool      `json:"notified"`
}

const wishlistCols = `id, tmdb_id, media_type, title, COALESCE(year,0), COALESCE(poster_url,''), COALESCE(overview,''), COALESCE(rating,0), COALESCE(genres,''), COALESCE(release_date::text,''), added_at, COALESCE(notified,false)`

func scanWishlistRow(scan func(...interface{}) error) (wishlistItem, bool) {
	var w wishlistItem
	if scan(&w.ID, &w.TmdbID, &w.MediaType, &w.Title, &w.Year, &w.PosterURL, &w.Overview, &w.Rating, &w.Genres, &w.ReleaseDate, &w.AddedAt, &w.Notified) != nil {
		return w, false
	}
	return w, true
}

func wishlistList() []wishlistItem {
	rows, err := db.Query(ctx, `SELECT `+wishlistCols+` FROM wishlist ORDER BY added_at DESC`)
	if err != nil {
		return []wishlistItem{}
	}
	defer rows.Close()
	items := []wishlistItem{}
	for rows.Next() {
		if w, ok := scanWishlistRow(rows.Scan); ok {
			items = append(items, w)
		}
	}
	return items
}

func wishlistAdd(tmdbID int, mediaType string) (*wishlistItem, error) {
	d, err := tmdbDetails(mediaType, tmdbID)
	if err != nil {
		return nil, err
	}
	title := d.Title
	if title == "" {
		title = d.Name
	}
	date := d.ReleaseDate
	if date == "" {
		date = d.FirstAirDate
	}
	year := 0
	if len(date) >= 4 {
		year, _ = strconv.Atoi(date[:4])
	}
	poster := ""
	if d.PosterPath != "" {
		poster = "https://image.tmdb.org/t/p/w500" + d.PosterPath
	}
	names := []string{}
	for _, g := range d.Genres {
		names = append(names, g.Name)
	}
	genres := strings.Join(names, ", ")

	_, err = db.Exec(ctx, `
		INSERT INTO wishlist (tmdb_id, media_type, title, year, poster_url, overview, rating, genres, release_date, added_at, notified)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')::date,NOW(),FALSE)
		ON CONFLICT (tmdb_id, media_type) DO UPDATE SET
			title=EXCLUDED.title, year=EXCLUDED.year, poster_url=EXCLUDED.poster_url,
			overview=EXCLUDED.overview, rating=EXCLUDED.rating, genres=EXCLUDED.genres,
			release_date=EXCLUDED.release_date`,
		tmdbID, mediaType, title, year, poster, d.Overview, d.VoteAverage, genres, date)
	if err != nil {
		return nil, err
	}
	var w wishlistItem
	err = db.QueryRow(ctx, `SELECT `+wishlistCols+` FROM wishlist WHERE tmdb_id=$1 AND media_type=$2`, tmdbID, mediaType).
		Scan(&w.ID, &w.TmdbID, &w.MediaType, &w.Title, &w.Year, &w.PosterURL, &w.Overview, &w.Rating, &w.Genres, &w.ReleaseDate, &w.AddedAt, &w.Notified)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// wishlistCheck отмечает вышедшие релизы (release_date <= today) и публикует события.
// onlyID > 0 — проверить только одну запись.
func wishlistCheck(onlyID int) []wishlistItem {
	query := `SELECT ` + wishlistCols + ` FROM wishlist
		WHERE COALESCE(notified,false)=FALSE AND release_date IS NOT NULL AND release_date <= CURRENT_DATE`
	args := []interface{}{}
	if onlyID > 0 {
		query += ` AND id=$1`
		args = append(args, onlyID)
	}
	query += ` ORDER BY release_date ASC`

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return []wishlistItem{}
	}
	released := []wishlistItem{}
	for rows.Next() {
		if w, ok := scanWishlistRow(rows.Scan); ok {
			released = append(released, w)
		}
	}
	rows.Close()

	for _, w := range released {
		_, _ = db.Exec(ctx, `UPDATE wishlist SET notified=TRUE WHERE id=$1`, w.ID)
		_ = rdb.Publish(ctx, "wishlist.released", w.Title).Err()
		_ = rdb.Incr(ctx, "wishlist:released_count").Err()
	}
	return released
}

func searchTMDB(mediaType, title string, year int) (*tmdbSearchResponse, error) {
	key := platform.GetEnv("TMDB_API_KEY", "")
	if key == "" {
		return nil, fmt.Errorf("TMDB_API_KEY не задан")
	}
	lang := platform.GetEnv("TMDB_LANG", "ru-RU")
	u := fmt.Sprintf("https://api.themoviedb.org/3/search/%s?api_key=%s&query=%s&language=%s", mediaType, key, url.QueryEscape(title), lang)
	if year > 0 {
		if mediaType == "tv" {
			u += fmt.Sprintf("&first_air_date_year=%d", year)
		} else {
			u += fmt.Sprintf("&year=%d", year)
		}
	}
	var res tmdbSearchResponse
	if err := httpGetJSON(u, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

type tmdbVideos struct {
	Results []struct {
		Key  string `json:"key"`
		Site string `json:"site"`
		Type string `json:"type"`
	} `json:"results"`
}

func tmdbTrailer(mediaType string, id int) string {
	cacheKey := fmt.Sprintf("tmdb:trailer:%s:%d", mediaType, id)
	if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		return cached
	}
	key := platform.GetEnv("TMDB_API_KEY", "")
	lang := platform.GetEnv("TMDB_LANG", "ru-RU")
	u := fmt.Sprintf("https://api.themoviedb.org/3/%s/%d/videos?api_key=%s&language=%s", mediaType, id, key, lang)
	var v tmdbVideos
	if err := httpGetJSON(u, &v); err != nil {
		_ = rdb.Set(ctx, cacheKey, "", 24*time.Hour).Err()
		return ""
	}
	trailer := ""
	for _, r := range v.Results {
		if r.Site == "YouTube" && (r.Type == "Trailer" || r.Type == "Teaser") {
			trailer = "https://www.youtube.com/watch?v=" + r.Key
			break
		}
	}
	_ = rdb.Set(ctx, cacheKey, trailer, 24*time.Hour).Err()
	return trailer
}

func tmdbDetails(mediaType string, id int) (*tmdbDetail, error) {
	key := platform.GetEnv("TMDB_API_KEY", "")
	lang := platform.GetEnv("TMDB_LANG", "ru-RU")
	u := fmt.Sprintf("https://api.themoviedb.org/3/%s/%d?api_key=%s&language=%s", mediaType, id, key, lang)
	var d tmdbDetail
	if err := httpGetJSON(u, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func lookupTMDB(mediaType, title string, year int) (*tmdbDetail, error) {
	cacheKey := fmt.Sprintf("tmdb:%s:%s:%d", mediaType, normalizeTitle(title), year)
	if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		if cached == "null" {
			return nil, nil
		}
		var d tmdbDetail
		if err := json.Unmarshal([]byte(cached), &d); err == nil {
			return &d, nil
		}
	}

	res, err := searchTMDB(mediaType, title, year)
	if err != nil {
		return nil, err
	}

	bestID := 0
	for _, r := range res.Results {
		rt := r.Title
		if rt == "" {
			rt = r.Name
		}
		if titleMatch(title, rt) {
			bestID = r.ID
			break
		}
	}

	if bestID == 0 {
		_ = rdb.Set(ctx, cacheKey, "null", 24*time.Hour).Err()
		return nil, nil
	}

	d, err := tmdbDetails(mediaType, bestID)
	if err != nil {
		return nil, err
	}
	if data, err := json.Marshal(d); err == nil {
		_ = rdb.Set(ctx, cacheKey, string(data), 24*time.Hour).Err()
	}
	return d, nil
}

// tmdbCredits — ответ эндпоинта TMDb .../credits.
type tmdbCredits struct {
	Cast []struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		ProfilePath string `json:"profile_path"`
		Character   string `json:"character"`
		Order       int    `json:"order"`
	} `json:"cast"`
	Crew []struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		ProfilePath string `json:"profile_path"`
		Job         string `json:"job"`
		Department  string `json:"department"`
	} `json:"crew"`
}

func profileURL(path string) string {
	if path == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w185" + path
}

// upsertPerson сохраняет персону и возвращает её внутренний id.
func upsertPerson(tmdbID int, name, profile string) (int, error) {
	if tmdbID <= 0 || name == "" {
		return 0, fmt.Errorf("invalid person")
	}
	var id int
	err := db.QueryRow(ctx, `
		INSERT INTO people (tmdb_id, name, profile_url, scanned_at)
		VALUES ($1,$2,$3,NOW())
		ON CONFLICT (tmdb_id) DO UPDATE SET name=EXCLUDED.name, profile_url=EXCLUDED.profile_url, scanned_at=NOW()
		RETURNING id`, tmdbID, name, profile).Scan(&id)
	return id, err
}

// syncCredits подтягивает актёров (топ-10) и режиссёра из TMDb для media.
func syncCredits(mediaID int, mediaType string, tmdbID int) {
	if mediaID <= 0 || tmdbID <= 0 {
		return
	}
	key := platform.GetEnv("TMDB_API_KEY", "")
	if key == "" {
		return
	}
	lang := platform.GetEnv("TMDB_LANG", "ru-RU")
	u := fmt.Sprintf("https://api.themoviedb.org/3/%s/%d/credits?api_key=%s&language=%s", mediaType, tmdbID, key, lang)

	var cr tmdbCredits
	err := httpGetJSON(u, &cr)
	if err != nil && strings.Contains(err.Error(), "429") {
		time.Sleep(time.Second)
		err = httpGetJSON(u, &cr)
	}
	if err != nil {
		log.Printf("⚠️ TMDb credits %s/%d: %v", mediaType, tmdbID, err)
		return
	}

	// начинаем с чистого листа для этого media
	_, _ = db.Exec(ctx, `DELETE FROM media_people WHERE media_id=$1`, mediaID)

	// актёры: топ-10 по порядку
	cast := cr.Cast
	sort.SliceStable(cast, func(i, j int) bool { return cast[i].Order < cast[j].Order })
	for i, a := range cast {
		if i >= 10 {
			break
		}
		pid, err := upsertPerson(a.ID, a.Name, profileURL(a.ProfilePath))
		if err != nil {
			continue
		}
		_, _ = db.Exec(ctx, `
			INSERT INTO media_people (media_id, person_id, role, character, ord)
			VALUES ($1,$2,'actor',$3,$4) ON CONFLICT DO NOTHING`, mediaID, pid, a.Character, i)
	}

	// режиссёр(ы)
	for _, c := range cr.Crew {
		if c.Job != "Director" {
			continue
		}
		pid, err := upsertPerson(c.ID, c.Name, profileURL(c.ProfilePath))
		if err != nil {
			continue
		}
		_, _ = db.Exec(ctx, `
			INSERT INTO media_people (media_id, person_id, role, character, ord)
			VALUES ($1,$2,'director',NULL,0) ON CONFLICT DO NOTHING`, mediaID, pid)
	}
}

// ============================================================
// VLC
// ============================================================

func openMedia(id int) error {
	var p, mediaType string
	if err := db.QueryRow(ctx, `SELECT path, media_type FROM media WHERE id=$1`, id).Scan(&p, &mediaType); err != nil {
		return fmt.Errorf("не найдено")
	}
	filePath := p
	if mediaType == "tv" {
		f, err := firstVideoFile(p)
		if err != nil {
			return err
		}
		filePath = f
	}

	hostPath := toHostPath(filePath, mediaType)
	openerURL := platform.GetEnv("BOOKS_OPENER_URL", "http://localhost:9999")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(openerURL + "/open-media?path=" + url.QueryEscape(hostPath))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opener: %s", string(body))
	}
	return nil
}

func openEpisode(id int) error {
	var path string
	if err := db.QueryRow(ctx, `SELECT path FROM episodes WHERE id=$1`, id).Scan(&path); err != nil {
		return fmt.Errorf("не найдено")
	}
	hostPath := toHostPath(path, "tv")
	openerURL := platform.GetEnv("BOOKS_OPENER_URL", "http://localhost:9999")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(openerURL + "/open-media?path=" + url.QueryEscape(hostPath))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opener: %s", string(body))
	}
	return nil
}

// toHostPath переводит контейнерный путь в хост-путь (для books-opener на хосте)
func toHostPath(containerPath, mediaType string) string {
	if mediaType == "tv" {
		hostDir := platform.GetEnv("MEDIA_TV_HOST_DIR", "/mnt/nas/root/FuzzyHD/TV")
		rel := strings.TrimPrefix(containerPath, platform.GetEnv("MEDIA_TV_DIR", "/tv"))
		return filepath.Join(hostDir, rel)
	}
	hostDir := platform.GetEnv("MEDIA_MOVIES_HOST_DIR", "/mnt/nas/root/FuzzyHD/Movies")
	rel := strings.TrimPrefix(containerPath, platform.GetEnv("MEDIA_MOVIES_DIR", "/movies"))
	return filepath.Join(hostDir, rel)
}

func firstVideoFile(dir string) (string, error) {
	var found string
	videoExts := map[string]bool{".mkv": true, ".mp4": true, ".avi": true}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" || d.IsDir() {
			return nil
		}
		if videoExts[strings.ToLower(filepath.Ext(p))] {
			found = p
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("нет видеофайлов в сериале")
	}
	return found, nil
}

// resolveMediaFile возвращает путь к файлу (для tv — первый видеофайл в папке)
func resolveMediaFile(id int) (string, string, error) {
	var p, mediaType string
	if err := db.QueryRow(ctx, `SELECT path, media_type FROM media WHERE id=$1`, id).Scan(&p, &mediaType); err != nil {
		return "", "", fmt.Errorf("не найдено")
	}
	if mediaType == "tv" {
		f, err := firstVideoFile(p)
		if err != nil {
			return "", "", err
		}
		return f, mediaType, nil
	}
	return p, mediaType, nil
}

// ============================================================
// ТРАНСКОД АУДИО
// ============================================================

func audioCodec(path string) string {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "csv=p=0",
		path,
	).Output()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(out)))
}

func videoCodec(path string) string {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "csv=p=0",
		path,
	).Output()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(out)))
}

// needsAudioTranscode — true, если аудио-кодек не воспроизводится браузером
func needsAudioTranscode(path string) bool {
	cacheKey := "transcode:audio:" + path
	if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		return cached == "true"
	}
	needs := false
	switch audioCodec(path) {
	case "ac3", "eac3", "dts", "truehd", "dts-hd", "mlp":
		needs = true
	}
	_ = rdb.Set(ctx, cacheKey, strconv.FormatBool(needs), 24*time.Hour).Err()
	return needs
}

// needsVideoTranscode — true, если видео-кодек не воспроизводится браузером
func needsVideoTranscode(path string) bool {
	cacheKey := "transcode:video:" + path
	if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
		return cached == "true"
	}
	needs := false
	switch videoCodec(path) {
	case "hevc", "h265", "av1", "vp9", "mpeg2video", "vc1":
		needs = true
	}
	_ = rdb.Set(ctx, cacheKey, strconv.FormatBool(needs), 24*time.Hour).Err()
	return needs
}

// detectVideoEncoder — определяет доступный H.264 энкодер (nvenc или cpu).
func detectVideoEncoder() string {
	mode := strings.ToLower(platform.GetEnv("TRANSCODE_ENCODER", "auto"))
	if mode == "cpu" {
		return "libx264"
	}
	if mode == "nvenc" {
		if probeNVENC() {
			return "h264_nvenc"
		}
		log.Println("⚠️ NVENC запрошен, но недоступен — использую libx264")
		return "libx264"
	}
	// auto
	if probeNVENC() {
		log.Println("🎥 Транскод HEVC: используется NVENC (GPU)")
		return "h264_nvenc"
	}
	log.Println("🎥 Транскод HEVC: используется libx264 (CPU)")
	return "libx264"
}

var (
	encoderOnce sync.Once
	encoderVal  string
)

func videoEncoder() string {
	encoderOnce.Do(func() {
		encoderVal = detectVideoEncoder()
	})
	return encoderVal
}

// probeNVENC проверяет реальную работу h264_nvenc коротким тестовым прогоном.
func probeNVENC() bool {
	c := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=128x72:d=0.1",
		"-frames:v", "1", "-c:v", "h264_nvenc", "-f", "null", "-")
	c.Stdout = io.Discard
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		return false
	}
	return true
}

func streamTranscoded(c fiber.Ctx, path string) error {
	args := []string{"-i", path}

	if platform.GetEnv("TRANSCODE_VIDEO_ENABLED", "true") != "true" {
		args = append(args, "-c:v", "copy")
	} else if needsVideoTranscode(path) {
		enc := videoEncoder()
		crf := platform.GetEnv("TRANSCODE_CRF", "23")
		preset := platform.GetEnv("TRANSCODE_PRESET", "veryfast")
		if enc == "h264_nvenc" {
			p := "p4"
			if preset != "" && preset != "veryfast" {
				p = preset
			}
			args = append(args, "-c:v", "h264_nvenc", "-preset", p, "-cq", crf)
		} else {
			args = append(args, "-c:v", "libx264", "-preset", preset, "-crf", crf, "-tune", "zerolatency")
		}
		// 10-bit / несовместимые пиксельные форматы → yuv420p для браузера
		args = append(args, "-pix_fmt", "yuv420p", "-profile:v", "high")
	} else {
		args = append(args, "-c:v", "copy")
	}

	// Аудио: всегда AAC для совместимости с браузером
	args = append(args,
		"-c:a", "aac", "-b:a", "192k", "-ac", "2",
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov",
		"pipe:1",
	)

	c.Set("X-Video-Transcode", strconv.FormatBool(needsVideoTranscode(path)))

	cmd := exec.Command("ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	go cmd.Wait()
	c.Set("Content-Type", "video/mp4")
	return c.SendStream(stdout)
}

// videoNeedsTranscodeJSON — вспомогательный эндпоинт для UI
func videoTranscodeInfo(c fiber.Ctx) error {
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
	}
	filePath, _, err := resolveMediaFile(id)
	if err != nil {
		return c.Status(404).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{
		"video_codec":     videoCodec(filePath),
		"audio_codec":     audioCodec(filePath),
		"video_transcode": needsVideoTranscode(filePath),
		"audio_transcode": needsAudioTranscode(filePath),
		"encoder":         videoEncoder(),
	})
}

// ============================================================
// ПАРСИНГ ИМЁН
// ============================================================

func parseName(name string) (string, int) {
	original := name

	// 1. Убрать расширение
	name = stripExtension(name)

	// 2. Заменить . и _ на пробелы
	name = strings.ReplaceAll(name, ".", " ")
	name = strings.ReplaceAll(name, "_", " ")

	// 3. Убрать числовой префикс ("01 Жуки" -> "Жуки")
	name = prefixRe.ReplaceAllString(name, "")

	// 4. Год — всё после отрезать
	year := 0
	if loc := yearRe.FindStringIndex(name); loc != nil {
		year, _ = strconv.Atoi(name[loc[0]:loc[1]])
		name = name[:loc[0]]
	}

	// 5-7. Теги, маркеры, нормализация пробелов
	title := cleanTitle(name)

	if title == "" {
		return original, year
	}
	return title, year
}

func stripExtension(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".avi", ".m4v", ".mov", ".wmv":
		return strings.TrimSuffix(name, filepath.Ext(name))
	}
	return name
}

func cleanTitle(s string) string {
	words := strings.Fields(s)
	kept := make([]string, 0, len(words))
	for _, w := range words {
		if isTag(w) {
			continue
		}
		kept = append(kept, w)
	}
	return strings.Join(kept, " ")
}

func isTag(w string) bool {
	trimmed := strings.Trim(w, "-()[]")
	lw := strings.ToLower(trimmed)
	if lw == "" {
		return true
	}
	if seasonRe.MatchString(lw) {
		return true
	}
	if tagsSet[lw] {
		return true
	}
	// Составные через дефис: "720p-ylnian", "BD-Remux"
	for _, part := range strings.Split(lw, "-") {
		if tagsSet[part] {
			return true
		}
	}
	return false
}

func parseEpisode(name string) (int, int, string) {
	base := stripExtension(name)

	type res struct {
		s, e int
		rest string
	}
	var r *res

	extract := func(re *regexp.Regexp) {
		loc := re.FindStringSubmatchIndex(base)
		if loc == nil || len(loc) < 6 {
			return
		}
		s, _ := strconv.Atoi(base[loc[2]:loc[3]])
		e, _ := strconv.Atoi(base[loc[4]:loc[5]])
		r = &res{s, e, base[loc[1]:]}
	}

	for _, re := range []*regexp.Regexp{epSE, epX, epSeason, epCyr, epBracket} {
		extract(re)
		if r != nil {
			break
		}
	}
	// паттерн 9: ведущий номер "01 Серия.mkv" — сезон неизвестен
	if r == nil {
		if m := epLeading.FindStringSubmatch(base); m != nil {
			e, _ := strconv.Atoi(m[1])
			r = &res{0, e, base[len(m[0]):]}
		}
	}
	if r == nil {
		return 0, 0, ""
	}

	rest := strings.ReplaceAll(r.rest, ".", " ")
	rest = strings.ReplaceAll(r.rest, "_", " ")
	rest = strings.Trim(rest, " -")
	return r.s, r.e, cleanTitle(rest)
}

func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '.', '_', '-', ':', '!', '?', ',', '\'', '’', '(', ')', '[', ']':
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func titleMatch(a, b string) bool {
	na := normalizeTitle(a)
	nb := normalizeTitle(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	return strings.Contains(na, nb) || strings.Contains(nb, na)
}

// ============================================================
// ПОДПИСКИ НА СЕРИАЛЫ
// ============================================================

func subscriptionList() []fiber.Map {
	rows, err := db.Query(ctx, `
		SELECT s.id, s.series_id, s.title, COALESCE(s.season,0), COALESCE(s.last_episode,0),
		       s.enabled, s.last_check
		FROM series_subscriptions s ORDER BY s.created_at DESC`)
	if err != nil {
		return []fiber.Map{}
	}
	defer rows.Close()
	items := []fiber.Map{}
	for rows.Next() {
		var id, seriesID, season, lastEp int
		var title string
		var enabled bool
		var lastCheck time.Time
		if rows.Scan(&id, &seriesID, &title, &season, &lastEp, &enabled, &lastCheck) == nil {
			items = append(items, fiber.Map{
				"id": id, "series_id": seriesID, "title": title, "season": season,
				"last_episode": lastEp, "enabled": enabled, "last_check": lastCheck,
			})
		}
	}
	return items
}

func subscriptionAdd(seriesID int) (fiber.Map, error) {
	var title string
	if err := db.QueryRow(ctx, `SELECT title FROM media WHERE id=$1 AND media_type='tv'`, seriesID).Scan(&title); err != nil {
		return nil, fmt.Errorf("сериал не найден")
	}
	_, err := db.Exec(ctx, `
		INSERT INTO series_subscriptions (series_id, title) VALUES ($1,$2)
		ON CONFLICT (series_id) DO UPDATE SET enabled=TRUE`, seriesID, title)
	if err != nil {
		return nil, err
	}
	return fiber.Map{"status": "subscribed", "series_id": seriesID, "title": title}, nil
}

type subSearchResult struct {
	Results []struct {
		Title       string `json:"title"`
		Magnet      string `json:"magnet"`
		DownloadURL string `json:"downloadUrl"`
	} `json:"results"`
}

// subscriptionCheck ищет новые серии и добавляет торренты.
func subscriptionCheck(id int) ([]string, error) {
	var seriesID int
	var title string
	if err := db.QueryRow(ctx, `SELECT series_id, title FROM series_subscriptions WHERE id=$1`, id).Scan(&seriesID, &title); err != nil {
		return nil, fmt.Errorf("подписка не найдена")
	}

	rows, err := db.Query(ctx, `
		SELECT season, MAX(episode) FROM episodes
		WHERE series_id=$1 GROUP BY season ORDER BY season`, seriesID)
	if err != nil {
		return nil, err
	}
	seasons := map[int]int{}
	for rows.Next() {
		var s, mx int
		if rows.Scan(&s, &mx) == nil {
			seasons[s] = mx
		}
	}
	rows.Close()

	searchURL := platform.GetEnv("SEARCH_SERVICE_URL", "http://search-service:8088")
	torrentURL := platform.GetEnv("TORRENT_SERVICE_URL", "http://torrent-service:8081")
	added := []string{}

	for season, maxEp := range seasons {
		next := maxEp + 1
		q := fmt.Sprintf("%s S%02dE%02d", title, season, next)
		var res subSearchResult
		u := fmt.Sprintf("%s/search?q=%s&limit=5", strings.TrimRight(searchURL, "/"), url.QueryEscape(q))
		if err := httpGetJSON(u, &res); err != nil || len(res.Results) == 0 {
			continue
		}
		r := res.Results[0]
		payload := map[string]string{}
		if r.Magnet != "" {
			payload["magnet"] = r.Magnet
		} else if r.DownloadURL != "" {
			payload["torrentUrl"] = r.DownloadURL
		} else {
			continue
		}
		b, _ := json.Marshal(payload)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, perr := client.Post(strings.TrimRight(torrentURL, "/")+"/add", "application/json", strings.NewReader(string(b)))
		if perr == nil {
			_ = resp.Body.Close()
			added = append(added, q)
			_ = rdb.Publish(ctx, "series.new_episode", fmt.Sprintf("%s S%02dE%02d", title, season, next)).Err()
		}
	}

	_, _ = db.Exec(ctx, `UPDATE series_subscriptions SET last_check=NOW() WHERE id=$1`, id)
	return added, nil
}
