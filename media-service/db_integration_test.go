//go:build integration

package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMediaLifecycleIntegration проверяет реальный CRUD в PostgreSQL.
// Запуск: TEST_DATABASE_URL=... go test -tags integration ./...
func TestMediaLifecycleIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL не задан")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	// Cleanup-функции выполняются LIFO: удаление строки — раньше закрытия пула.
	t.Cleanup(pool.Close)

	// Таблица создаётся media-service при старте. Если её нет — создаём
	// минимальную схему, достаточную для проверки CRUD.
	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS media (
		id SERIAL PRIMARY KEY,
		path TEXT UNIQUE NOT NULL,
		media_type VARCHAR(16) NOT NULL,
		title TEXT NOT NULL,
		year INT,
		tmdb_id INT,
		rating REAL,
		genres TEXT,
		runtime INT,
		added_at TIMESTAMP DEFAULT NOW())`)
	if err != nil {
		t.Fatalf("create media: %v", err)
	}

	path := fmt.Sprintf("/test/integration-%d.mkv", time.Now().UnixNano())
	var id int
	err = pool.QueryRow(ctx,
		`INSERT INTO media (path, media_type, title, year, rating, genres)
		 VALUES ($1, 'movie', $2, $3, $4, $5) RETURNING id`,
		path, "Integration Movie", 2026, 7.5, "драма").Scan(&id)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM media WHERE id=$1`, id)
	})

	var title, mediaType, genres string
	var year int
	var rating float64
	err = pool.QueryRow(ctx,
		`SELECT title, media_type, year, rating, genres FROM media WHERE id=$1`, id).
		Scan(&title, &mediaType, &year, &rating, &genres)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if title != "Integration Movie" || mediaType != "movie" || year != 2026 || genres != "драма" {
		t.Fatalf("unexpected row: title=%q type=%q year=%d genres=%q", title, mediaType, year, genres)
	}
	if math.Abs(rating-7.5) > 1e-4 {
		t.Fatalf("rating = %v, want 7.5", rating)
	}

	// UPDATE + повторное чтение.
	if _, err := pool.Exec(ctx, `UPDATE media SET rating=$1 WHERE id=$2`, 9.1, id); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT rating FROM media WHERE id=$1`, id).Scan(&rating); err != nil {
		t.Fatalf("select after update: %v", err)
	}
	if math.Abs(rating-9.1) > 1e-4 {
		t.Fatalf("rating after update = %v, want 9.1", rating)
	}

	// DELETE + проверка, что строки больше нет.
	if _, err := pool.Exec(ctx, `DELETE FROM media WHERE id=$1`, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM media WHERE id=$1`, id).Scan(&count); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("row still present after delete: count=%d", count)
	}
}
