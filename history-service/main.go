package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
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

func main() {
	if err := platform.ValidateEnv([]string{"POSTGRES_URL"}); err != nil {
		log.Fatalf("❌ %v", err)
	}
	// Подключаемся к PostgreSQL
	pgURL := platform.GetEnv("POSTGRES_URL", "postgres://nas:nas_secret@localhost:5432/nas_history?sslmode=disable")
	var err error
	db, err = pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatalf("❌ PostgreSQL: %v", err)
	}
	defer db.Close()

	// Создаём таблицы
	if err := migrate(); err != nil {
		log.Fatalf("❌ Migration: %v", err)
	}

	// Подключаемся к Redis
	rdb = redis.NewClient(&redis.Options{
		Addr: platform.GetEnv("REDIS_ADDR", "localhost:6379"),
	})

	// Запускаем слушателя событий в фоне
	go listenEvents()

	// HTTP-сервер
	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error {
		if err := db.Ping(ctx); err != nil {
			return c.Status(500).JSON(fiber.Map{"status": "error", "db": err.Error()})
		}
		return c.JSON(platform.HealthJSON("history", nil))
	})

	// История событий (с фильтрами)
	app.Get("/events", func(c fiber.Ctx) error {
		limit, _ := strconv.Atoi(c.Query("limit", "50"))
		if limit <= 0 || limit > 500 {
			limit = 50
		}
		offset, _ := strconv.Atoi(c.Query("offset", "0"))
		if offset < 0 {
			offset = 0
		}

		query := `SELECT id, type, payload, created_at FROM events`
		args := []interface{}{}
		where := []string{}
		arg := 1
		if t := c.Query("type"); t != "" {
			where = append(where, fmt.Sprintf("type = $%d", arg))
			args = append(args, t)
			arg++
		}
		if s := c.Query("since"); s != "" {
			where = append(where, fmt.Sprintf("created_at >= $%d", arg))
			args = append(args, s)
			arg++
		}
		if u := c.Query("until"); u != "" {
			where = append(where, fmt.Sprintf("created_at <= $%d", arg))
			args = append(args, u)
			arg++
		}
		if len(where) > 0 {
			query += " WHERE " + strings.Join(where, " AND ")
		}
		query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", arg, arg+1)
		args = append(args, limit, offset)

		rows, err := db.Query(ctx, query, args...)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()

		var events []fiber.Map
		for rows.Next() {
			var id int
			var typ, payload string
			var createdAt time.Time
			if err := rows.Scan(&id, &typ, &payload, &createdAt); err != nil {
				continue
			}
			events = append(events, fiber.Map{
				"id":         id,
				"type":       typ,
				"payload":    payload,
				"created_at": createdAt,
			})
		}

		return c.JSON(fiber.Map{"events": events, "count": len(events)})
	})

	// Поиск по событиям
	app.Get("/events/search", func(c fiber.Ctx) error {
		q := c.Query("q")
		if q == "" {
			return c.Status(400).JSON(fiber.Map{"error": "q is required"})
		}
		rows, err := db.Query(ctx, `SELECT id, type, payload, created_at FROM events WHERE payload ILIKE '%' || $1 || '%' ORDER BY created_at DESC LIMIT 100`, q)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		var events []fiber.Map
		for rows.Next() {
			var id int
			var typ, payload string
			var createdAt time.Time
			if rows.Scan(&id, &typ, &payload, &createdAt) == nil {
				events = append(events, fiber.Map{"id": id, "type": typ, "payload": payload, "created_at": createdAt})
			}
		}
		return c.JSON(fiber.Map{"events": events, "count": len(events)})
	})

	// Очистить события
	app.Delete("/events", func(c fiber.Ctx) error {
		if c.Query("all") == "1" {
			db.Exec(ctx, `DELETE FROM events`)
		} else {
			db.Exec(ctx, `DELETE FROM events WHERE created_at < NOW() - INTERVAL '30 days'`)
		}
		return c.JSON(fiber.Map{"status": "cleared"})
	})

	// Статистика по дням
	app.Get("/stats/daily", func(c fiber.Ctx) error {
		days, _ := strconv.Atoi(c.Query("days", "30"))
		if days <= 0 || days > 365 {
			days = 30
		}
		rows, err := db.Query(ctx, `SELECT to_char(created_at, 'YYYY-MM-DD') AS d, COUNT(*) FROM events WHERE created_at > NOW() - make_interval(days => $1) GROUP BY d ORDER BY d`, days)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		daily := []fiber.Map{}
		for rows.Next() {
			var d string
			var c int
			if rows.Scan(&d, &c) == nil {
				daily = append(daily, fiber.Map{"date": d, "count": c})
			}
		}
		return c.JSON(fiber.Map{"daily": daily})
	})

	// Статистика
	app.Get("/stats", func(c fiber.Ctx) error {
		var total int
		db.QueryRow(ctx, `SELECT COUNT(*) FROM events`).Scan(&total)

		var today int
		db.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM events WHERE created_at > NOW() - INTERVAL '1 day'`,
		).Scan(&today)

		var week int
		db.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM events WHERE created_at > NOW() - INTERVAL '7 days'`,
		).Scan(&week)

		// Топ типов событий
		rows, _ := db.Query(
			ctx,
			`SELECT type, COUNT(*) FROM events GROUP BY type ORDER BY COUNT(*) DESC LIMIT 10`,
		)
		defer rows.Close()

		byType := []fiber.Map{}
		for rows.Next() {
			var typ string
			var count int
			rows.Scan(&typ, &count)
			byType = append(byType, fiber.Map{"type": typ, "count": count})
		}

		return c.JSON(fiber.Map{
			"total":   total,
			"today":   today,
			"week":    week,
			"by_type": byType,
		})
	})

	// Статистика скачиваний
	app.Get("/stats/downloads", func(c fiber.Ctx) error {
		days, _ := strconv.Atoi(c.Query("days", "30"))
		if days <= 0 || days > 365 {
			days = 30
		}
		rows, err := db.Query(ctx, `
			SELECT to_char(date,'YYYY-MM-DD'), COALESCE(SUM(count),0), COALESCE(SUM(size_bytes),0)
			FROM download_stats
			WHERE date > CURRENT_DATE - make_interval(days => $1)
			GROUP BY date ORDER BY date`, days)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		daily := []fiber.Map{}
		var total int
		for rows.Next() {
			var d string
			var cnt int
			var sz int64
			if rows.Scan(&d, &cnt, &sz) == nil {
				total += cnt
				daily = append(daily, fiber.Map{"date": d, "count": cnt, "size_bytes": sz})
			}
		}
		var allTime int
		_ = db.QueryRow(ctx, `SELECT COALESCE(SUM(count),0) FROM download_stats`).Scan(&allTime)
		return c.JSON(fiber.Map{"daily": daily, "total": total, "all_time": allTime, "days": days})
	})

	app.Get("/stats/indexers", func(c fiber.Ctx) error {
		rows, err := db.Query(ctx, `
			SELECT COALESCE(indexer,'unknown'), COALESCE(SUM(count),0)
			FROM download_stats GROUP BY indexer ORDER BY SUM(count) DESC LIMIT 10`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		for rows.Next() {
			var n string
			var cnt int
			if rows.Scan(&n, &cnt) == nil {
				items = append(items, fiber.Map{"indexer": n, "count": cnt})
			}
		}
		return c.JSON(fiber.Map{"indexers": items})
	})

	app.Get("/stats/categories", func(c fiber.Ctx) error {
		rows, err := db.Query(ctx, `
			SELECT COALESCE(category,'unknown'), COALESCE(SUM(count),0)
			FROM download_stats GROUP BY category ORDER BY SUM(count) DESC LIMIT 10`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		for rows.Next() {
			var n string
			var cnt int
			if rows.Scan(&n, &cnt) == nil {
				items = append(items, fiber.Map{"category": n, "count": cnt})
			}
		}
		return c.JSON(fiber.Map{"categories": items})
	})

	// Статистика музыки
	app.Get("/music/stats/top-artists", func(c fiber.Ctx) error {
		days, _ := strconv.Atoi(c.Query("days", "30"))
		if days <= 0 || days > 365 {
			days = 30
		}
		rows, err := db.Query(ctx, `
			SELECT COALESCE(artist,'—'), COALESCE(SUM(count),0)
			FROM music_stats WHERE date > CURRENT_DATE - make_interval(days => $1)
			GROUP BY artist ORDER BY SUM(count) DESC LIMIT 10`, days)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		for rows.Next() {
			var n string
			var cnt int
			if rows.Scan(&n, &cnt) == nil {
				items = append(items, fiber.Map{"artist": n, "count": cnt})
			}
		}
		return c.JSON(fiber.Map{"artists": items, "days": days})
	})

	app.Get("/music/stats/top-tracks", func(c fiber.Ctx) error {
		days, _ := strconv.Atoi(c.Query("days", "30"))
		if days <= 0 || days > 365 {
			days = 30
		}
		rows, err := db.Query(ctx, `
			SELECT COALESCE(artist,''), COALESCE(title,''), COALESCE(SUM(count),0)
			FROM music_stats WHERE date > CURRENT_DATE - make_interval(days => $1)
			GROUP BY artist, title ORDER BY SUM(count) DESC LIMIT 10`, days)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		for rows.Next() {
			var a, t string
			var cnt int
			if rows.Scan(&a, &t, &cnt) == nil {
				items = append(items, fiber.Map{"artist": a, "title": t, "count": cnt})
			}
		}
		return c.JSON(fiber.Map{"tracks": items, "days": days})
	})

	app.Get("/music/stats/total", func(c fiber.Ctx) error {
		var plays int
		_ = db.QueryRow(ctx, `SELECT COALESCE(SUM(count),0) FROM music_stats`).Scan(&plays)
		// оценка: средний трек ~3.5 мин
		hours := float64(plays) * 3.5 / 60.0
		return c.JSON(fiber.Map{"total_plays": plays, "estimated_hours": hours})
	})

	log.Println("📊 history-service на :8085")
	log.Fatal(app.Listen(":8085"))
}

func migrate() error {
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS events (
			id SERIAL PRIMARY KEY,
			type VARCHAR(64) NOT NULL,
			payload TEXT,
			created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_events_type ON events(type);
		CREATE INDEX IF NOT EXISTS idx_events_created_at ON events(created_at);

		CREATE TABLE IF NOT EXISTS download_stats (
			id SERIAL PRIMARY KEY,
			date DATE NOT NULL,
			indexer VARCHAR(64),
			category VARCHAR(32),
			size_bytes BIGINT DEFAULT 0,
			count INT DEFAULT 1,
			UNIQUE(date, indexer, category)
		);

		CREATE TABLE IF NOT EXISTS music_stats (
			id SERIAL PRIMARY KEY,
			date DATE NOT NULL,
			artist TEXT,
			title TEXT,
			album TEXT,
			file_path TEXT,
			count INT DEFAULT 1,
			UNIQUE(date, file_path)
		);

		-- Миграция старых записей: сырой magnet -> читаемый текст.
		-- .* — жадный поиск последнего dn= (у rutor в magnet два dn=).
		UPDATE events
		SET payload = '📥 Добавлен торрент: ' ||
			COALESCE(substring(payload from '.*dn=([^&]+)'),
			         '<magnet ' || substring(payload from 'btih:([a-fA-F0-9]{8})') || '…>')
		WHERE type = 'torrent.added' AND payload LIKE 'magnet:%';
	`)
	return err
}

func listenEvents() {
	pubsub := rdb.Subscribe(
		ctx,
		"torrent.added",
		"torrent.completed",
		"disk.warning",
		"media.new_file",
		"backup.completed",
		"wishlist.added",
		"wishlist.released",
		"series.new_episode",
		"music.played",
	)
	defer pubsub.Close()
	ch := pubsub.Channel()

	log.Println("👀 history-service слушает Redis...")

	for msg := range ch {
		log.Printf("📥 [%s] %s", msg.Channel, msg.Payload)
		if err := saveEvent(msg.Channel, msg.Payload); err != nil {
			log.Printf("❌ save failed: %v", err)
		}
	}
}

func saveEvent(typ, payload string) error {
	formatted := formatPayload(typ, payload)
	_, err := db.Exec(
		ctx,
		`INSERT INTO events (type, payload) VALUES ($1, $2)`,
		typ, formatted,
	)
	if typ == "torrent.added" {
		storeDownloadStat(payload)
	}
	if typ == "music.played" {
		storeMusicStat(payload)
	}
	return err
}

// storeMusicStat агрегирует статистику прослушивания музыки.
func storeMusicStat(raw string) {
	var m struct {
		Artist string `json:"artist"`
		Title  string `json:"title"`
		Album  string `json:"album"`
		File   string `json:"file"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m.File == "" {
		return
	}
	_, _ = db.Exec(ctx, `
		INSERT INTO music_stats (date, artist, title, album, file_path, count)
		VALUES (CURRENT_DATE, $1, $2, $3, $4, 1)
		ON CONFLICT (date, file_path) DO UPDATE SET count = music_stats.count + 1`,
		m.Artist, m.Title, m.Album, m.File)
}

// storeDownloadStat агрегирует статистику скачиваний торрентов по дням.
func storeDownloadStat(raw string) {
	cat := guessDownloadCategory(raw)
	_, _ = db.Exec(ctx, `
		INSERT INTO download_stats (date, indexer, category, size_bytes, count)
		VALUES (CURRENT_DATE, 'prowlarr', $1, 0, 1)
		ON CONFLICT (date, indexer, category) DO UPDATE SET count = download_stats.count + 1`,
		cat)
}

func guessDownloadCategory(title string) string {
	t := strings.ToLower(title)
	switch {
	case reSeason.MatchString(t):
		return "tv"
	case strings.Contains(t, "flac") || strings.Contains(t, "mp3") || strings.Contains(t, "lossless") ||
		strings.Contains(t, "dsd") || strings.Contains(t, "discography") || strings.Contains(t, "дискография"):
		return "music"
	case strings.Contains(t, "fitgirl") || strings.Contains(t, "repack") || strings.Contains(t, "xatab") || strings.Contains(t, "gog"):
		return "games"
	case strings.Contains(t, "udemy") || strings.Contains(t, "course") || strings.Contains(t, "курс"):
		return "courses"
	case strings.Contains(t, "book") || strings.Contains(t, "книг") || strings.Contains(t, ".pdf") || strings.Contains(t, ".epub") || strings.Contains(t, ".fb2") || strings.Contains(t, ".mobi"):
		return "books"
	default:
		return "movies"
	}
}

var reSeason = regexp.MustCompile(`(?i)s\d{1,2}\s?e\d{1,2}|season\s?\d|сезон\s?\d`)

// formatPayload превращает сырой payload события в человекочитаемый текст
func formatPayload(typ, raw string) string {
	switch typ {
	case "torrent.added":
		return "📥 Добавлен торрент: " + prettyMagnet(raw)
	case "torrent.completed":
		return "✅ Завершён торрент: " + prettyMagnet(raw)
	case "disk.warning":
		return "⚠️ Диск: " + truncate(raw, 200)
	case "media.new_file":
		return formatNewFile(raw)
	case "backup.completed":
		return "💾 Бэкап БД: " + truncate(raw, 200)
	case "wishlist.added":
		return "❤️ В список желаемого: " + truncate(raw, 200)
	case "wishlist.released":
		return "🎬 Вышел фильм: " + truncate(raw, 200)
	case "series.new_episode":
		return "📺 Новая серия: " + truncate(raw, 200)
	case "music.played":
		var m struct {
			Artist string `json:"artist"`
			Title  string `json:"title"`
		}
		if json.Unmarshal([]byte(raw), &m) == nil && m.Title != "" {
			return "🎵 " + truncate(m.Artist+" — "+m.Title, 200)
		}
		return "🎵 " + truncate(raw, 200)
	default:
		return truncate(raw, 200)
	}
}

// formatNewFile превращает "kind|filename" в человекочитаемый текст
func formatNewFile(raw string) string {
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		return "🆕 Новый файл: " + truncate(raw, 200)
	}
	kind, name := parts[0], parts[1]
	var icon, label string
	switch kind {
	case "movie":
		icon, label = "🎬", "Новый фильм"
	case "tv":
		icon, label = "📺", "Новая серия"
	case "photo":
		icon, label = "🖼️", "Новое фото"
	default:
		icon, label = "🆕", "Новый файл"
	}
	return icon + " " + label + ": " + truncate(name, 200)
}

// prettyMagnet превращает magnet-ссылку в читаемое имя торрента
func prettyMagnet(s string) string {
	if !strings.HasPrefix(s, "magnet:") {
		return truncate(s, 120)
	}
	if name := magnetDisplayName(s); name != "" {
		return truncate(name, 120)
	}
	if hash := magnetHashPrefix(s); hash != "" {
		return "<magnet " + hash + "…>"
	}
	return truncate(s, 120)
}

// magnetDisplayName извлекает и декодирует параметр dn= из magnet-ссылки.
// Берём ПОСЛЕДНИЙ dn= — у rutor в magnet два dn= (имя сайта и название топика),
// и нам нужен самый специфичный.
func magnetDisplayName(s string) string {
	idx := strings.LastIndex(s, "dn=")
	if idx == -1 {
		return ""
	}
	rest := s[idx+3:]
	if amp := strings.Index(rest, "&"); amp != -1 {
		rest = rest[:amp]
	}
	if decoded, err := url.QueryUnescape(rest); err == nil {
		return strings.ReplaceAll(decoded, "+", " ")
	}
	return strings.ReplaceAll(rest, "+", " ")
}

// magnetHashPrefix извлекает первые 16 символов btih-хеша
func magnetHashPrefix(s string) string {
	idx := strings.Index(s, "btih:")
	if idx == -1 {
		return ""
	}
	rest := s[idx+5:]
	if amp := strings.Index(rest, "&"); amp != -1 {
		rest = rest[:amp]
	}
	if len(rest) > 16 {
		rest = rest[:16]
	}
	return rest
}

// truncate обрезает строку до max символов (по рунам, безопасно для UTF-8)
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

var _ = json.Marshal
