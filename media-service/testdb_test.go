package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDBTimeout = 30 * time.Second

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL не задан")
	}
	return dsn
}

func randomSchemaSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// testPool поднимает пул, привязанный к одноразовой схеме в TEST_DATABASE_URL,
// и применяет к ней production-миграцию. Схема удаляется после теста, поэтому
// тесты не мешают друг другу и не зависят от состояния public.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL: invalid DSN")
	}

	// Деструктивный guard: DDL/DML выполняются только в *_test базе.
	dbName := cfg.ConnConfig.Database
	if !strings.HasSuffix(dbName, "_test") && os.Getenv("TEST_DB_ALLOW_NONTEST") != "1" {
		t.Fatalf("TEST_DATABASE_URL указывает на БД %q: нужен суффикс _test (override: TEST_DB_ALLOW_NONTEST=1)", dbName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	t.Cleanup(cancel)

	admin, err := pgx.ConnectConfig(ctx, cfg.ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect %s: %v", cfg.ConnConfig.Host, err)
	}
	schema := "test_" + randomSchemaSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	_ = admin.Close(ctx)

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	// Cleanup LIFO: DROP выполнится раньше pool.Close.
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), testDBTimeout)
		defer dropCancel()
		if _, err := pool.Exec(dropCtx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping %s: %v", cfg.ConnConfig.Host, err)
	}
	if err := migrateWith(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// useTestDB подменяет пакетный db на время теста.
// Тесты с этим helper нельзя запускать с t.Parallel().
func useTestDB(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	prev := db
	db = pool
	t.Cleanup(func() { db = prev })
}
