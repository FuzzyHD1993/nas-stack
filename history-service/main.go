package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

var (
	db  *pgxpool.Pool
	rdb *redis.Client
	ctx = context.Background()

	// HISTORY_GATEWAY_TOKEN — сервисный доступ gateway -> history.
	// HISTORY_ADMIN_TOKEN — только для DELETE /events.
	// Оба читаются один раз на старте до запуска HTTP-сервера.
	gatewayToken string
	adminToken   string

	listenerOK atomic.Bool
)

const (
	dbTimeout         = 5 * time.Second
	maxEventTextRunes = 200
	maxPayloadRunes   = 220
	maxSearchRunes    = 200
	maxOffset         = 100_000
)

var eventChannels = []string{
	"torrent.added",
	"torrent.completed",
	"disk.warning",
	"media.new_file",
	"backup.completed",
	"wishlist.added",
	"wishlist.released",
	"series.new_episode",
	"music.played",
}

func main() {
	if err := platform.ValidateEnv([]string{"POSTGRES_URL"}); err != nil {
		log.Fatalf("❌ %v", err)
	}

	// HISTORY_GATEWAY_TOKEN имеет приоритет, NOTIFY_GATEWAY_TOKEN — fallback,
	// чтобы не требовать новой переменной при раскатке.
	gatewayToken = strings.TrimSpace(platform.GetEnv("HISTORY_GATEWAY_TOKEN", ""))
	if gatewayToken == "" {
		gatewayToken = strings.TrimSpace(platform.GetEnv("NOTIFY_GATEWAY_TOKEN", ""))
	}
	adminToken = strings.TrimSpace(platform.GetEnv("HISTORY_ADMIN_TOKEN", ""))
	if adminToken == "" {
		adminToken = strings.TrimSpace(platform.GetEnv("ADMIN_TOKEN", ""))
	}
	if gatewayToken == "" {
		log.Println("⚠️ HISTORY_GATEWAY_TOKEN/NOTIFY_GATEWAY_TOKEN не задан — все ручки кроме /health и /metrics вернут 503")
	}
	if adminToken == "" {
		log.Println("⚠️ HISTORY_ADMIN_TOKEN/ADMIN_TOKEN не задан — DELETE /events недоступен")
	}

	// Подключаемся к PostgreSQL
	pgURL := platform.GetEnv("POSTGRES_URL", "")
	pgCfg, err := pgxpool.ParseConfig(pgURL)
	if err != nil {
		log.Fatalf("❌ PostgreSQL config: %v", err)
	}
	pgCfg.MaxConns = 8
	pgCfg.MaxConnIdleTime = 5 * time.Minute
	pgCfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pgCfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10000"

	db, err = pgxpool.NewWithConfig(ctx, pgCfg)
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
	app := fiber.New(fiber.Config{
		AppName:      "NAS History",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
		BodyLimit:    1 << 20,
	})
	platform.StartPprof()
	// metrics снаружи recover: паника превращается в err и фиксируется как 500.
	app.Use(metricsMiddleware())
	app.Use(recover.New())
	app.Use(func(c fiber.Ctx) error {
		c.Set("X-Content-Type-Options", "nosniff")
		return c.Next()
	})

	// /health нужен docker healthcheck, /metrics — prometheus (порт наружу
	// не публикуется). Всё остальное доступно только gateway.
	app.Use(func(c fiber.Ctx) error {
		switch c.Path() {
		case "/health", "/metrics":
			return c.Next()
		}
		return requireGatewayToken(c)
	})

	app.Get("/health", func(c fiber.Ctx) error {
		reqCtx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
		defer cancel()

		dbOK := db.Ping(reqCtx) == nil
		redisOK := rdb.Ping(reqCtx).Err() == nil
		listening := listenerOK.Load()

		resp := platform.HealthJSON("history", fiber.Map{
			"db":       dbOK,
			"redis":    redisOK,
			"listener": listening,
		})
		if !dbOK || !redisOK || !listening {
			resp["status"] = "error"
			return c.Status(fiber.StatusServiceUnavailable).JSON(resp)
		}
		return c.JSON(resp)
	})

	app.Get("/metrics", adaptor.HTTPHandler(metricsHandler()))

	// История событий (с фильтрами)
	app.Get("/events", func(c fiber.Ctx) error {
		limit := clampInt(c.Query("limit"), 50, 1, 500)
		offset := clampInt(c.Query("offset"), 0, 0, maxOffset)

		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		query := `SELECT id, type, COALESCE(payload,''), created_at FROM events`
		args := []any{}
		where := []string{}
		arg := 1

		if t := strings.TrimSpace(c.Query("type")); t != "" {
			where = append(where, fmt.Sprintf("type = $%d", arg))
			args = append(args, t)
			arg++
		}
		if s := strings.TrimSpace(c.Query("since")); s != "" {
			ts, err := parseTimeParam(s, false)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid since"})
			}
			where = append(where, fmt.Sprintf("created_at >= $%d", arg))
			args = append(args, ts)
			arg++
		}
		if u := strings.TrimSpace(c.Query("until")); u != "" {
			ts, err := parseTimeParam(u, true)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid until"})
			}
			where = append(where, fmt.Sprintf("created_at <= $%d", arg))
			args = append(args, ts)
			arg++
		}
		if len(where) > 0 {
			query += " WHERE " + strings.Join(where, " AND ")
		}
		query += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", arg, arg+1)
		args = append(args, limit, offset)

		rows, err := db.Query(reqCtx, query, args...)
		if err != nil {
			return internalError(c, "events query", err)
		}
		events, err := scanEvents(rows)
		if err != nil {
			return internalError(c, "events scan", err)
		}
		return c.JSON(fiber.Map{"events": events, "count": len(events)})
	})

	// Поиск по событиям
	app.Get("/events/search", func(c fiber.Ctx) error {
		q := strings.TrimSpace(c.Query("q"))
		if q == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "q is required"})
		}
		if utf8.RuneCountInString(q) > maxSearchRunes {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "q too long"})
		}

		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		rows, err := db.Query(reqCtx, `
			SELECT id, type, COALESCE(payload,''), created_at
			FROM events
			WHERE payload ILIKE '%' || $1 || '%' ESCAPE '\'
			ORDER BY created_at DESC, id DESC
			LIMIT 100`, escapeLike(q))
		if err != nil {
			return internalError(c, "events search", err)
		}
		events, err := scanEvents(rows)
		if err != nil {
			return internalError(c, "events search scan", err)
		}
		return c.JSON(fiber.Map{"events": events, "count": len(events)})
	})

	// Очистить события — только с ADMIN_TOKEN. Удаление всей истории требует
	// явного confirm=delete-all.
	app.Delete("/events", requireAdminToken, func(c fiber.Ctx) error {
		reqCtx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
		defer cancel()

		var deleted int64
		if c.Query("all") == "1" {
			if c.Query("confirm") != "delete-all" {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "confirm=delete-all required"})
			}
			tag, err := db.Exec(reqCtx, `DELETE FROM events`)
			if err != nil {
				return internalError(c, "delete all events", err)
			}
			deleted = tag.RowsAffected()
		} else {
			tag, err := db.Exec(reqCtx, `DELETE FROM events WHERE created_at < NOW() - INTERVAL '30 days'`)
			if err != nil {
				return internalError(c, "delete old events", err)
			}
			deleted = tag.RowsAffected()
		}
		log.Printf("⚠️ history: удалено событий: %d", deleted)
		return c.JSON(fiber.Map{"status": "cleared", "deleted": deleted})
	})

	// Статистика по дням
	app.Get("/stats/daily", func(c fiber.Ctx) error {
		days := clampInt(c.Query("days"), 30, 1, 365)
		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		rows, err := db.Query(reqCtx, `
			SELECT to_char(created_at, 'YYYY-MM-DD') AS d, COUNT(*)
			FROM events
			WHERE created_at > NOW() - make_interval(days => $1)
			GROUP BY d ORDER BY d`, days)
		if err != nil {
			return internalError(c, "stats daily", err)
		}
		defer rows.Close()

		daily := []fiber.Map{}
		for rows.Next() {
			var d string
			var count int
			if err := rows.Scan(&d, &count); err != nil {
				return internalError(c, "stats daily scan", err)
			}
			daily = append(daily, fiber.Map{"date": d, "count": count})
		}
		if err := rows.Err(); err != nil {
			return internalError(c, "stats daily rows", err)
		}
		return c.JSON(fiber.Map{"daily": daily})
	})

	// Статистика
	app.Get("/stats", func(c fiber.Ctx) error {
		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		var total, today, week int
		if err := db.QueryRow(reqCtx, `
			SELECT
				COUNT(*),
				COUNT(*) FILTER (WHERE created_at > NOW() - INTERVAL '1 day'),
				COUNT(*) FILTER (WHERE created_at > NOW() - INTERVAL '7 days')
			FROM events`).Scan(&total, &today, &week); err != nil {
			return internalError(c, "stats summary", err)
		}

		rows, err := db.Query(reqCtx,
			`SELECT type, COUNT(*) FROM events GROUP BY type ORDER BY COUNT(*) DESC LIMIT 10`)
		if err != nil {
			return internalError(c, "stats by_type", err)
		}
		defer rows.Close()

		byType := []fiber.Map{}
		for rows.Next() {
			var typ string
			var count int
			if err := rows.Scan(&typ, &count); err != nil {
				return internalError(c, "stats by_type scan", err)
			}
			byType = append(byType, fiber.Map{"type": typ, "count": count})
		}
		if err := rows.Err(); err != nil {
			return internalError(c, "stats by_type rows", err)
		}

		return c.JSON(fiber.Map{
			"total": total, "today": today, "week": week, "by_type": byType,
		})
	})

	// Статистика скачиваний
	app.Get("/stats/downloads", func(c fiber.Ctx) error {
		days := clampInt(c.Query("days"), 30, 1, 365)
		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		rows, err := db.Query(reqCtx, `
			SELECT to_char(date,'YYYY-MM-DD'), COALESCE(SUM(count),0), COALESCE(SUM(size_bytes),0)
			FROM download_stats
			WHERE date > CURRENT_DATE - make_interval(days => $1)
			GROUP BY date ORDER BY date`, days)
		if err != nil {
			return internalError(c, "stats downloads", err)
		}
		defer rows.Close()

		daily := []fiber.Map{}
		total := 0
		for rows.Next() {
			var d string
			var count int
			var size int64
			if err := rows.Scan(&d, &count, &size); err != nil {
				return internalError(c, "stats downloads scan", err)
			}
			total += count
			daily = append(daily, fiber.Map{"date": d, "count": count, "size_bytes": size})
		}
		if err := rows.Err(); err != nil {
			return internalError(c, "stats downloads rows", err)
		}

		var allTime int
		if err := db.QueryRow(reqCtx,
			`SELECT COALESCE(SUM(count),0) FROM download_stats`).Scan(&allTime); err != nil {
			return internalError(c, "stats downloads all_time", err)
		}
		return c.JSON(fiber.Map{"daily": daily, "total": total, "all_time": allTime, "days": days})
	})

	app.Get("/stats/indexers", func(c fiber.Ctx) error {
		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		rows, err := db.Query(reqCtx, `
			SELECT COALESCE(indexer,'unknown') AS name, COALESCE(SUM(count),0) AS cnt
			FROM download_stats GROUP BY 1 ORDER BY 2 DESC LIMIT 10`)
		if err != nil {
			return internalError(c, "stats indexers", err)
		}
		items, err := scanPairs(rows, "indexer")
		if err != nil {
			return internalError(c, "stats indexers scan", err)
		}
		return c.JSON(fiber.Map{"indexers": items})
	})

	app.Get("/stats/categories", func(c fiber.Ctx) error {
		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		rows, err := db.Query(reqCtx, `
			SELECT COALESCE(category,'unknown') AS name, COALESCE(SUM(count),0) AS cnt
			FROM download_stats GROUP BY 1 ORDER BY 2 DESC LIMIT 10`)
		if err != nil {
			return internalError(c, "stats categories", err)
		}
		items, err := scanPairs(rows, "category")
		if err != nil {
			return internalError(c, "stats categories scan", err)
		}
		return c.JSON(fiber.Map{"categories": items})
	})

	// Статистика музыки
	app.Get("/music/stats/top-artists", func(c fiber.Ctx) error {
		days := clampInt(c.Query("days"), 30, 1, 365)
		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		rows, err := db.Query(reqCtx, `
			SELECT COALESCE(artist,'—') AS name, COALESCE(SUM(count),0) AS cnt
			FROM music_stats WHERE date > CURRENT_DATE - make_interval(days => $1)
			GROUP BY 1 ORDER BY 2 DESC LIMIT 10`, days)
		if err != nil {
			return internalError(c, "music top-artists", err)
		}
		items, err := scanPairs(rows, "artist")
		if err != nil {
			return internalError(c, "music top-artists scan", err)
		}
		return c.JSON(fiber.Map{"artists": items, "days": days})
	})

	app.Get("/music/stats/top-tracks", func(c fiber.Ctx) error {
		days := clampInt(c.Query("days"), 30, 1, 365)
		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		rows, err := db.Query(reqCtx, `
			SELECT COALESCE(artist,''), COALESCE(title,''), COALESCE(SUM(count),0)
			FROM music_stats WHERE date > CURRENT_DATE - make_interval(days => $1)
			GROUP BY artist, title ORDER BY SUM(count) DESC LIMIT 10`, days)
		if err != nil {
			return internalError(c, "music top-tracks", err)
		}
		defer rows.Close()

		items := []fiber.Map{}
		for rows.Next() {
			var artist, title string
			var count int
			if err := rows.Scan(&artist, &title, &count); err != nil {
				return internalError(c, "music top-tracks scan", err)
			}
			items = append(items, fiber.Map{"artist": artist, "title": title, "count": count})
		}
		if err := rows.Err(); err != nil {
			return internalError(c, "music top-tracks rows", err)
		}
		return c.JSON(fiber.Map{"tracks": items, "days": days})
	})

	app.Get("/music/stats/total", func(c fiber.Ctx) error {
		reqCtx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		var plays int
		if err := db.QueryRow(reqCtx,
			`SELECT COALESCE(SUM(count),0) FROM music_stats`).Scan(&plays); err != nil {
			return internalError(c, "music total", err)
		}
		// оценка: средний трек ~3.5 мин
		hours := float64(plays) * 3.5 / 60.0
		return c.JSON(fiber.Map{"total_plays": plays, "estimated_hours": hours})
	})

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := app.Listen(":8085"); err != nil {
			log.Printf("❌ HTTP listener: %v", err)
			stop()
		}
	}()
	log.Println("📊 history-service на :8085")

	<-sigCtx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		log.Printf("⚠️ shutdown: %v", err)
	}
	if err := rdb.Close(); err != nil {
		log.Printf("⚠️ redis close: %v", err)
	}
}

func migrate() error {
	mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err := db.Exec(mctx, `
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
		CREATE INDEX IF NOT EXISTS idx_download_stats_date ON download_stats(date);

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
		CREATE INDEX IF NOT EXISTS idx_music_stats_date ON music_stats(date);

		-- Миграция старых записей: сырой magnet -> читаемый текст.
		-- .* — жадный поиск последнего dn= (у rutor в magnet два dn=).
		-- COALESCE с непустым fallback: NULL в payload ломает Scan в string.
		UPDATE events
		SET payload = '📥 Добавлен торрент: ' ||
			COALESCE(
				NULLIF(substring(payload from '.*dn=([^&]+)'), ''),
				'magnet ' || COALESCE(substring(payload from 'btih:([a-fA-F0-9]{8})'), '') || '…'
			)
		WHERE type = 'torrent.added'
		  AND payload LIKE 'magnet:%'
		  AND payload IS NOT NULL;
	`)
	return err
}

func listenEvents() {
	for {
		err := listenOnce()
		listenerOK.Store(false)
		if err != nil {
			log.Printf("⚠️ redis listener: %v (перезапуск через 5s)", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func listenOnce() error {
	pubsub := rdb.Subscribe(ctx, eventChannels...)
	defer pubsub.Close()

	subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := pubsub.Receive(subCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	listenerOK.Store(true)
	log.Println("👀 history-service слушает Redis...")

	ch := pubsub.Channel(redis.WithChannelSize(2048))
	for msg := range ch {
		if msg == nil {
			continue
		}
		// В лог — только санитизированное и усечённое значение.
		log.Printf("📥 [%s] %s", msg.Channel, safeLog(msg.Payload, maxEventTextRunes))
		if err := saveEvent(msg.Channel, msg.Payload); err != nil {
			log.Printf("❌ save failed: %v", err)
		}
	}
	return fmt.Errorf("redis pubsub channel closed")
}

func saveEvent(typ, payload string) error {
	formatted := formatPayload(typ, payload)

	saveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := db.Exec(saveCtx,
		`INSERT INTO events (type, payload) VALUES ($1, $2)`,
		typ, formatted,
	); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	switch typ {
	case "torrent.added":
		if err := storeDownloadStat(saveCtx, payload); err != nil {
			log.Printf("❌ download stats: %v", err)
		}
	case "music.played":
		if err := storeMusicStat(saveCtx, payload); err != nil {
			log.Printf("❌ music stats: %v", err)
		}
	}
	return nil
}

// storeMusicStat агрегирует статистику прослушивания музыки.
func storeMusicStat(qctx context.Context, raw string) error {
	var m struct {
		Artist string `json:"artist"`
		Title  string `json:"title"`
		Album  string `json:"album"`
		File   string `json:"file"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m.File == "" {
		return nil
	}

	_, err := db.Exec(qctx, `
		INSERT INTO music_stats (date, artist, title, album, file_path, count)
		VALUES (CURRENT_DATE, $1, $2, $3, $4, 1)
		ON CONFLICT (date, file_path) DO UPDATE
		SET count = music_stats.count + 1,
		    artist = EXCLUDED.artist,
		    title  = EXCLUDED.title,
		    album  = EXCLUDED.album`,
		truncate(sanitizeText(m.Artist), 512),
		truncate(sanitizeText(m.Title), 512),
		truncate(sanitizeText(m.Album), 512),
		truncate(sanitizeText(m.File), 1024),
	)
	return err
}

// storeDownloadStat агрегирует статистику скачиваний торрентов по дням.
func storeDownloadStat(qctx context.Context, raw string) error {
	// Категорию считаем по декодированному имени, а не по сырому magnet-URL.
	cat := guessDownloadCategory(prettyMagnet(raw))
	_, err := db.Exec(qctx, `
		INSERT INTO download_stats (date, indexer, category, size_bytes, count)
		VALUES (CURRENT_DATE, 'prowlarr', $1, 0, 1)
		ON CONFLICT (date, indexer, category) DO UPDATE SET count = download_stats.count + 1`,
		cat)
	return err
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

// formatPayload превращает сырой payload события в человекочитаемый текст.
// Управляющие символы вырезаются, итоговая длина ограничена.
func formatPayload(typ, raw string) string {
	raw = sanitizeText(raw)

	switch typ {
	case "torrent.added":
		return truncate("📥 Добавлен торрент: "+prettyMagnet(raw), maxPayloadRunes)
	case "torrent.completed":
		return truncate("✅ Завершён торрент: "+prettyMagnet(raw), maxPayloadRunes)
	case "disk.warning":
		return truncate("⚠️ Диск: "+raw, maxPayloadRunes)
	case "media.new_file":
		return formatNewFile(raw)
	case "backup.completed":
		return truncate("💾 Бэкап БД: "+raw, maxPayloadRunes)
	case "wishlist.added":
		return truncate("❤️ В список желаемого: "+raw, maxPayloadRunes)
	case "wishlist.released":
		return truncate("🎬 Вышел фильм: "+raw, maxPayloadRunes)
	case "series.new_episode":
		return truncate("📺 Новая серия: "+raw, maxPayloadRunes)
	case "music.played":
		var m struct {
			Artist string `json:"artist"`
			Title  string `json:"title"`
		}
		if json.Unmarshal([]byte(raw), &m) == nil && m.Title != "" {
			return truncate("🎵 "+sanitizeText(m.Artist+" — "+m.Title), maxPayloadRunes)
		}
		return truncate("🎵 "+raw, maxPayloadRunes)
	default:
		return truncate(raw, maxPayloadRunes)
	}
}

// formatNewFile превращает "kind|filename" в человекочитаемый текст
func formatNewFile(raw string) string {
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		return truncate("🆕 Новый файл: "+raw, maxPayloadRunes)
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
	return truncate(icon+" "+label+": "+sanitizeText(name), maxPayloadRunes)
}

// prettyMagnet превращает magnet-ссылку в читаемое имя торрента
func prettyMagnet(s string) string {
	if !strings.HasPrefix(s, "magnet:") {
		return truncate(s, 120)
	}
	if name := magnetDisplayName(s); name != "" {
		return truncate(sanitizeText(name), 120)
	}
	if hash := magnetHashPrefix(s); hash != "" {
		return "magnet " + hash + "…"
	}
	return truncate(s, 120)
}

// magnetDisplayName извлекает параметр dn= через url.ParseQuery.
// ParseQuery сам декодирует %XX и "+" -> " ", повторная замена "+" не нужна.
func magnetDisplayName(s string) string {
	const prefix = "magnet:?"
	if !strings.HasPrefix(s, prefix) {
		return ""
	}
	q, err := url.ParseQuery(s[len(prefix):])
	if err != nil && len(q["dn"]) == 0 {
		return ""
	}
	names := q["dn"]
	if len(names) == 0 {
		return ""
	}
	// Берём ПОСЛЕДНИЙ dn= — у rutor в magnet два dn=.
	return strings.TrimSpace(names[len(names)-1])
}

var btihRe = regexp.MustCompile(`(?i)btih:([a-f0-9]{40}|[a-z2-7]{32})`)

// magnetHashPrefix извлекает первые 16 символов btih-хеша.
// Принимаются только hex-40 и base32-32: произвольная строка (в т.ч. HTML)
// в «хеш» не попадает.
func magnetHashPrefix(s string) string {
	m := btihRe.FindStringSubmatch(s)
	if len(m) != 2 {
		return ""
	}
	h := m[1]
	if len(h) > 16 {
		h = h[:16]
	}
	return h
}

var ctrlRe = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// sanitizeText делает строку безопасной для JSON/логов: валидный UTF-8 без C0-контролов.
func sanitizeText(s string) string {
	return ctrlRe.ReplaceAllString(strings.ToValidUTF8(s, "�"), "")
}

// truncate обрезает строку до max рун, не аллоцируя []rune от всей строки.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

func internalError(c fiber.Ctx, msg string, err error) error {
	if err != nil {
		log.Printf("❌ %s: %v", msg, err)
	}
	return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
}

func tokenEqual(got, expected string) bool {
	if got == "" || expected == "" {
		return false
	}
	g := sha256.Sum256([]byte(got))
	e := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(g[:], e[:]) == 1
}

func requireGatewayToken(c fiber.Ctx) error {
	if gatewayToken == "" {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "service token not configured"})
	}
	if !tokenEqual(strings.TrimSpace(c.Get("X-Gateway-Token")), gatewayToken) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	return c.Next()
}

func requireAdminToken(c fiber.Ctx) error {
	if !tokenEqual(strings.TrimSpace(c.Get("X-Admin-Token")), adminToken) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "admin token required"})
	}
	return c.Next()
}

func clampInt(raw string, def, min, max int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < min {
		return def
	}
	if n > max {
		return max
	}
	return n
}

func parseTimeParam(v string, endOfDay bool) (time.Time, error) {
	v = strings.TrimSpace(v)
	if ts, err := time.Parse(time.RFC3339, v); err == nil {
		return ts, nil
	}
	ts, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		return ts.Add(24*time.Hour - time.Nanosecond), nil
	}
	return ts, nil
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func escapeLike(s string) string {
	return likeEscaper.Replace(s)
}

func scanEvents(rows pgx.Rows) ([]fiber.Map, error) {
	defer rows.Close()

	events := []fiber.Map{}
	for rows.Next() {
		var id int
		var typ, payload string
		var createdAt time.Time
		if err := rows.Scan(&id, &typ, &payload, &createdAt); err != nil {
			return nil, err
		}
		events = append(events, fiber.Map{
			"id":         id,
			"type":       typ,
			"payload":    payload,
			"created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func scanPairs(rows pgx.Rows, label string) ([]fiber.Map, error) {
	defer rows.Close()

	items := []fiber.Map{}
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		items = append(items, fiber.Map{label: name, "count": count})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func safeLog(s string, max int) string {
	return truncate(sanitizeText(s), max)
}
