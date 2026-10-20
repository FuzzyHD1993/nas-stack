//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Запуск:
//
//	TEST_DATABASE_URL=postgres://.../nas_history_test?sslmode=disable \
//	  go test -tags integration ./...
func TestMediaLifecycleIntegration(t *testing.T) {
	pool := testPool(t)
	useTestDB(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	path := fmt.Sprintf("/test/%s-%d.mkv", t.Name(), time.Now().UnixNano())

	var id int
	if err := pool.QueryRow(ctx, `
		INSERT INTO media (path, media_type, title, year, rating, genres)
		VALUES ($1,'movie',$2,$3,$4,$5) RETURNING id`,
		path, "Integration Movie", 2026, 7.5, "драма").Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// UNIQUE(path) — повторный скан не должен создавать вторую строку.
	_, err := pool.Exec(ctx, `INSERT INTO media (path, media_type, title) VALUES ($1,'movie','dup')`, path)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("ожидали unique violation 23505, получили: %v", err)
	}

	var m mediaItem
	if err := pool.QueryRow(ctx, `SELECT `+mediaCols+` FROM media WHERE id=$1`, id).
		Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID,
			&m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime); err != nil {
		t.Fatalf("select: %v", err)
	}
	if m.Title != "Integration Movie" || m.MediaType != "movie" || m.Year != 2026 || m.Genres != "драма" {
		t.Fatalf("unexpected row: %+v", m)
	}
	if math.Abs(m.Rating-7.5) > 1e-4 {
		t.Fatalf("rating = %v, want 7.5", m.Rating)
	}
	if got, err := mediaIDByPath(path); err != nil || got != id {
		t.Fatalf("mediaIDByPath = %d, %v; want %d", got, err, id)
	}

	var addedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT added_at FROM media WHERE id=$1`, id).Scan(&addedAt); err != nil || addedAt.IsZero() {
		t.Fatalf("added_at = %v, %v", addedAt, err)
	}

	tag, err := pool.Exec(ctx, `UPDATE media SET rating=$1 WHERE id=$2`, 9.1, id)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("update affected %d rows, want 1", tag.RowsAffected())
	}
	if err := pool.QueryRow(ctx, `SELECT rating FROM media WHERE id=$1`, id).Scan(&m.Rating); err != nil {
		t.Fatalf("select after update: %v", err)
	}
	if math.Abs(m.Rating-9.1) > 1e-4 {
		t.Fatalf("rating after update = %v, want 9.1", m.Rating)
	}

	tag, err = pool.Exec(ctx, `DELETE FROM media WHERE id=$1`, id)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete affected %d rows, want 1", tag.RowsAffected())
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM media WHERE id=$1`, id).Scan(&count); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("row still present after delete: count=%d", count)
	}
}
