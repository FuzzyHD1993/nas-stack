package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	maxProfileNameRunes = 32
	maxAvatarRunes      = 8
	queryTimeout        = 5 * time.Second
)

type profile struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	IsKids    bool   `json:"is_kids"`
	MaxRating string `json:"max_rating"`
	Avatar    string `json:"avatar"`
	HasPIN    bool   `json:"has_pin"`
}

// migrationLockID — ключ pg_advisory_lock: реплики не гоняются за DDL/seed.
const migrationLockID int64 = 0x6e61735f70726f66 // "nas_prof"

func connectProfilesDB(ctx context.Context) *pgxpool.Pool {
	// Никаких дефолтных кредов в коде: без POSTGRES_URL профили недоступны.
	pgURL := strings.TrimSpace(platform.GetEnv("POSTGRES_URL", ""))
	if pgURL == "" {
		log.Printf("⚠️ POSTGRES_URL не задан — профили недоступны")
		return nil
	}

	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Printf("⚠️ PostgreSQL: %v (профили недоступны)", err)
		return nil
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Printf("⚠️ PostgreSQL: %v (профили недоступны)", err)
		pool.Close()
		return nil
	}

	// playback_progress принадлежит media-service — не создаём чужую таблицу.
	if err := migrateProfiles(ctx, pool); err != nil {
		log.Printf("⚠️ Миграция profiles: %v", err)
		pool.Close()
		return nil
	}

	return pool
}

// migrateProfiles выполняет DDL и seed под advisory lock в одной сессии:
// при рестарте/нескольких репликах нет гонки CREATE/INSERT.
func migrateProfiles(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS profiles (
			id SERIAL PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			pin_hash TEXT,
			is_kids BOOLEAN DEFAULT FALSE,
			max_rating TEXT DEFAULT 'R',
			avatar_emoji TEXT DEFAULT '👤',
			created_at TIMESTAMPTZ DEFAULT NOW()
		);`); err != nil {
		return err
	}

	// Сид только в пустую таблицу: удалённый/переименованный профиль не воскресает.
	_, err = conn.Exec(ctx, `
		INSERT INTO profiles (name, avatar_emoji)
		SELECT 'Я', '👤'
		WHERE NOT EXISTS (SELECT 1 FROM profiles)`)
	return err
}

func hashPIN(pin string) (string, error) {
	if pin == "" {
		return "", nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// legacyHashPIN — проверка хэшей, созданных до перехода на bcrypt
// (sha256 со статической «солью»).
func legacyHashPIN(pin string) string {
	h := sha256.Sum256([]byte("nas-stack:" + pin))
	return hex.EncodeToString(h[:])
}

func verifyPIN(stored, pin string) (ok bool, needsRehash bool) {
	switch {
	case stored == "":
		// PIN не задан — профиль открыт.
		return true, false
	case strings.HasPrefix(stored, "$2"):
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(pin)) == nil, false
	default:
		return subtle.ConstantTimeCompare([]byte(legacyHashPIN(pin)), []byte(stored)) == 1, true
	}
}

func validPIN(pin string) bool {
	if len(pin) < 4 || len(pin) > 8 {
		return false
	}
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return false
		}
	}
	return true
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func profileRoutes(app *fiber.App, db *pgxpool.Pool, cfg config) {
	auth := requireAdmin(cfg)

	selectLimiter := limiter.New(limiter.Config{
		Max:        10,
		Expiration: time.Minute,
		KeyGenerator: func(c fiber.Ctx) string {
			return clientKey(c) + "|" + c.Params("id")
		},
		LimitReached: func(c fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "too many attempts"})
		},
	})

	app.Get("/profiles", auth, listProfiles(db))
	app.Post("/profiles", auth, createProfile(db))
	app.Post("/profiles/:id/select", auth, selectLimiter, selectProfile(db))
}

func listProfiles(db *pgxpool.Pool) fiber.Handler {
	return func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "БД недоступна"})
		}

		ctx, cancel := context.WithTimeout(c.Context(), queryTimeout)
		defer cancel()

		rows, err := db.Query(ctx,
			`SELECT id, name, is_kids, COALESCE(max_rating,'R'), COALESCE(avatar_emoji,'👤'),
			        COALESCE(pin_hash,'') <> ''
			   FROM profiles ORDER BY id`)
		if err != nil {
			log.Printf("profiles: list: %v", err)
			return internalError(c)
		}
		defer rows.Close()

		out := []profile{}
		for rows.Next() {
			var p profile
			if err := rows.Scan(&p.ID, &p.Name, &p.IsKids, &p.MaxRating, &p.Avatar, &p.HasPIN); err != nil {
				log.Printf("profiles: scan: %v", err)
				return internalError(c)
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			log.Printf("profiles: rows: %v", err)
			return internalError(c)
		}
		return c.JSON(fiber.Map{"profiles": out})
	}
}

func createProfile(db *pgxpool.Pool) fiber.Handler {
	return func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "БД недоступна"})
		}

		var req struct {
			Name      string `json:"name"`
			PIN       string `json:"pin"`
			IsKids    bool   `json:"is_kids"`
			MaxRating string `json:"max_rating"`
			Avatar    string `json:"avatar"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
		}

		name := strings.TrimSpace(req.Name)
		if name == "" || utf8.RuneCountInString(name) > maxProfileNameRunes || hasControlChars(name) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid name"})
		}
		if req.PIN != "" && !validPIN(req.PIN) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "pin must be 4-8 digits"})
		}

		rating := strings.ToUpper(strings.TrimSpace(req.MaxRating))
		if rating == "" {
			rating = "R"
		}
		switch rating {
		case "G", "PG", "PG-13", "R", "NC-17":
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid max_rating"})
		}
		if req.IsKids && (rating == "R" || rating == "NC-17") {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid max_rating for kids profile"})
		}

		avatar := strings.TrimSpace(req.Avatar)
		if avatar == "" {
			avatar = "👤"
		}
		if utf8.RuneCountInString(avatar) > maxAvatarRunes || hasControlChars(avatar) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid avatar"})
		}

		pinHash, err := hashPIN(req.PIN)
		if err != nil {
			log.Printf("profiles: hash pin: %v", err)
			return internalError(c)
		}
		var pinValue any
		if pinHash != "" {
			pinValue = pinHash
		}

		ctx, cancel := context.WithTimeout(c.Context(), queryTimeout)
		defer cancel()

		var id int
		err = db.QueryRow(ctx,
			`INSERT INTO profiles (name, pin_hash, is_kids, max_rating, avatar_emoji)
			 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			name, pinValue, req.IsKids, rating, avatar).Scan(&id)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "profile name already exists"})
			}
			log.Printf("profiles: create: %v", err)
			return internalError(c)
		}
		return c.JSON(fiber.Map{"status": "created", "id": id})
	}
}

func selectProfile(db *pgxpool.Pool) fiber.Handler {
	return func(c fiber.Ctx) error {
		// id парсим до обращения к БД и используем одно и то же число везде.
		pid, err := strconv.Atoi(c.Params("id"))
		if err != nil || pid <= 0 || pid > math.MaxInt32 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid profile id"})
		}

		if db == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "БД недоступна"})
		}

		var req struct {
			PIN string `json:"pin"`
		}
		if len(c.Body()) > 0 {
			if err := c.Bind().JSON(&req); err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
			}
		}

		ctx, cancel := context.WithTimeout(c.Context(), queryTimeout)
		defer cancel()

		var (
			p       profile
			pinHash string
		)
		err = db.QueryRow(ctx,
			`SELECT id, name, is_kids, COALESCE(max_rating,'R'),
			        COALESCE(avatar_emoji,'👤'), COALESCE(pin_hash,'')
			   FROM profiles WHERE id=$1`, pid).
			Scan(&p.ID, &p.Name, &p.IsKids, &p.MaxRating, &p.Avatar, &pinHash)
		if errors.Is(err, pgx.ErrNoRows) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "profile not found"})
		}
		if err != nil {
			log.Printf("profiles: select id=%d: %v", pid, err)
			return internalError(c)
		}

		// Не гоняем bcrypt/legacy-хеш по многомегабайтному PIN.
		if pinHash != "" && (req.PIN == "" || !validPIN(req.PIN)) {
			log.Printf("profiles: invalid pin format id=%d ip=%s", pid, clientKey(c))
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid pin"})
		}

		ok, needsRehash := verifyPIN(pinHash, req.PIN)
		if !ok {
			log.Printf("profiles: invalid pin id=%d ip=%s", pid, clientKey(c))
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid pin"})
		}
		p.HasPIN = pinHash != ""

		if needsRehash {
			if newHash, err := hashPIN(req.PIN); err == nil && newHash != "" {
				if _, err := db.Exec(ctx, `UPDATE profiles SET pin_hash=$1 WHERE id=$2`, newHash, pid); err != nil {
					log.Printf("profiles: rehash pin id=%d: %v", pid, err)
				}
			}
		}

		// Токен здесь больше не выпускается: профиль — это UI-состояние,
		// а не отдельный уровень доступа к gateway.
		c.Set("Cache-Control", "no-store")
		return c.JSON(fiber.Map{"profile_id": p.ID, "profile": p})
	}
}
