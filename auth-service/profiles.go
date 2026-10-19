package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type profile struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	IsKids    bool   `json:"is_kids"`
	MaxRating string `json:"max_rating"`
	Avatar    string `json:"avatar"`
	HasPIN    bool   `json:"has_pin"`
}

func connectProfilesDB(ctx context.Context) *pgxpool.Pool {
	pgURL := platform.GetEnv("POSTGRES_URL", "postgres://nas:nas_secret@postgres:5432/nas_history?sslmode=disable")
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Printf("⚠️ PostgreSQL: %v (профили недоступны)", err)
		return nil
	}
	_, err = pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS profiles (
			id SERIAL PRIMARY KEY,
			name TEXT UNIQUE NOT NULL,
			pin_hash TEXT,
			is_kids BOOLEAN DEFAULT FALSE,
			max_rating TEXT DEFAULT 'R',
			avatar_emoji TEXT DEFAULT '👤',
			created_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS playback_progress (
			media_type TEXT, media_id INT, position_sec INT, duration_sec INT,
			updated_at TIMESTAMPTZ, profile_id INT DEFAULT 1,
			PRIMARY KEY (media_type, media_id)
		);
		ALTER TABLE playback_progress ADD COLUMN IF NOT EXISTS profile_id INT DEFAULT 1;`)
	if err != nil {
		log.Printf("⚠️ Миграция profiles: %v", err)
	}
	_, _ = pool.Exec(ctx, `INSERT INTO profiles (name, avatar_emoji) VALUES ('Я','👤') ON CONFLICT (name) DO NOTHING`)
	return pool
}

func hashPIN(pin string) string {
	if pin == "" {
		return ""
	}
	h := sha256.Sum256([]byte("nas-stack:" + pin))
	return hex.EncodeToString(h[:])
}

func signToken(secret []byte, profileID int) (string, error) {
	claims := jwt.MapClaims{
		"sub":        "user",
		"profile_id": profileID,
		"iat":        time.Now().Unix(),
		"exp":        time.Now().Add(30 * 24 * time.Hour).Unix(),
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(secret)
}

func profileRoutes(app *fiber.App, db *pgxpool.Pool, secret []byte) {
	app.Get("/profiles", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		rows, err := db.Query(context.Background(),
			`SELECT id, name, is_kids, COALESCE(max_rating,'R'), COALESCE(avatar_emoji,'👤'), COALESCE(pin_hash,'') <> '' FROM profiles ORDER BY id`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		out := []profile{}
		for rows.Next() {
			var p profile
			if rows.Scan(&p.ID, &p.Name, &p.IsKids, &p.MaxRating, &p.Avatar, &p.HasPIN) == nil {
				out = append(out, p)
			}
		}
		return c.JSON(fiber.Map{"profiles": out})
	})

	app.Post("/profiles", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		var req struct {
			Name      string `json:"name"`
			PIN       string `json:"pin"`
			IsKids    bool   `json:"is_kids"`
			MaxRating string `json:"max_rating"`
			Avatar    string `json:"avatar"`
		}
		if err := c.Bind().JSON(&req); err != nil || req.Name == "" {
			return c.Status(400).JSON(fiber.Map{"error": "name required"})
		}
		if req.MaxRating == "" {
			req.MaxRating = "R"
		}
		if req.Avatar == "" {
			req.Avatar = "👤"
		}
		var id int
		err := db.QueryRow(context.Background(),
			`INSERT INTO profiles (name, pin_hash, is_kids, max_rating, avatar_emoji)
			 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			req.Name, hashPIN(req.PIN), req.IsKids, req.MaxRating, req.Avatar).Scan(&id)
		if err != nil {
			return c.Status(409).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "created", "id": id})
	})

	app.Post("/profiles/:id/select", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		id := c.Params("id")
		var req struct {
			PIN string `json:"pin"`
		}
		_ = c.Bind().JSON(&req)
		var pinHash string
		if err := db.QueryRow(context.Background(), `SELECT COALESCE(pin_hash,'') FROM profiles WHERE id=$1`, id).Scan(&pinHash); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "profile not found"})
		}
		if pinHash != "" && hashPIN(req.PIN) != pinHash {
			return c.Status(401).JSON(fiber.Map{"error": "invalid pin"})
		}
		token, err := signToken(secret, atoiSafe(id))
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"token": token, "profile_id": atoiSafe(id)})
	})
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
