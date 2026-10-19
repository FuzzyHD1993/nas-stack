package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Тесты БД выполняются только если задан TEST_DATABASE_URL (в CI — сервис postgres).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL не задан")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func TestMediaInsertSelect(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	// Таблица создаётся media-service при старте; если её нет — создаём минимальную.
	_, _ = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS media (
		id SERIAL PRIMARY KEY, path TEXT UNIQUE NOT NULL, media_type VARCHAR(16) NOT NULL,
		title TEXT NOT NULL, year INT, tmdb_id INT, rating REAL, genres TEXT, runtime INT,
		added_at TIMESTAMP DEFAULT NOW())`)

	var id int
	err := pool.QueryRow(ctx, `INSERT INTO media (path,media_type,title) VALUES ($1,'movie','Test Movie') RETURNING id`,
		"/test/v10.3-movie.mkv").Scan(&id)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	defer pool.Exec(ctx, `DELETE FROM media WHERE id=$1`, id)

	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM media WHERE id=$1`, id).Scan(&title); err != nil {
		t.Fatalf("select: %v", err)
	}
	if title != "Test Movie" {
		t.Errorf("title = %q, want %q", title, "Test Movie")
	}
}
