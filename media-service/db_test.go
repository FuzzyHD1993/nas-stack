package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMediaInsertSelect(t *testing.T) {
	pool := testPool(t)
	useTestDB(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	path := fmt.Sprintf("/test/%s-%d.mkv", t.Name(), time.Now().UnixNano())

	var id int
	if err := pool.QueryRow(ctx,
		`INSERT INTO media (path, media_type, title) VALUES ($1,'movie','Test Movie') RETURNING id`,
		path).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Читаем production-запросом: NULL-колонки не должны ломать Scan.
	var m mediaItem
	if err := pool.QueryRow(ctx, `SELECT `+mediaCols+` FROM media WHERE id=$1`, id).
		Scan(&m.ID, &m.MediaType, &m.Title, &m.OriginalTitle, &m.Year, &m.TMDBID,
			&m.Overview, &m.PosterURL, &m.BackdropURL, &m.Rating, &m.Genres, &m.Runtime); err != nil {
		t.Fatalf("select mediaCols: %v", err)
	}
	if m.Title != "Test Movie" || m.MediaType != "movie" {
		t.Fatalf("unexpected row: %+v", m)
	}
	if m.Year != 0 || m.Rating != 0 || m.Genres != "" || m.PosterURL != "" || m.Overview != "" {
		t.Fatalf("NULL-колонки не нормализованы: %+v", m)
	}

	if got, err := mediaIDByPath(path); err != nil || got != id {
		t.Fatalf("mediaIDByPath = %d, %v; want %d", got, err, id)
	}
}
