package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

type Task struct {
	Name     string
	Schedule time.Duration
	Command  string
	Args     []string
}

var tasks = []Task{
	{
		Name:     "backup-configs",
		Schedule: 24 * time.Hour,
		Command:  "fish",
		Args:     []string{"-c", "backupme"},
	},
	{
		Name:     "flac-check",
		Schedule: 7 * 24 * time.Hour,
		Command:  "fish",
		Args:     []string{"-c", "flac-fix"},
	},
	{
		Name:     "nas-mount",
		Schedule: 1 * time.Hour,
		Command:  "fish",
		Args:     []string{"-c", "nas-mount"},
	},
	{
		Name:     "pg-backup",
		Schedule: 24 * time.Hour,
	},
	{
		Name:     "gitea-backup",
		Schedule: 24 * time.Hour,
	},
	{
		Name:     "wishlist-release-check",
		Schedule: 6 * time.Hour,
	},
	{
		Name:     "series-check",
		Schedule: 6 * time.Hour,
	},
	{
		Name:     "healthcheck-services",
		Schedule: 1 * time.Minute,
	},
	{
		Name:     "disk-check-detailed",
		Schedule: 6 * time.Hour,
	},
	{
		Name:     "memory-check",
		Schedule: 5 * time.Minute,
	},
	{
		Name:     "oom-check",
		Schedule: 15 * time.Minute,
	},
	{
		Name:     "metrics-retention",
		Schedule: 24 * time.Hour,
	},
}

var (
	rdb *redis.Client
	db  *pgxpool.Pool
	ctx = context.Background()

	hostAgentClient = &http.Client{
		Timeout: 10 * time.Second,
		// X-Admin-Token не должен уехать на чужой хост через 302.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	prowlarrClient = &http.Client{
		Timeout: 20 * time.Second,
		// Без SSRF-пивота через 3xx.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	notifyHTTPClient = &http.Client{Timeout: 10 * time.Second}
)

var updateStatusCache struct {
	mu   sync.Mutex
	at   time.Time
	body fiber.Map
}

// fetchUpdateStatus спрашивает host-agent с таймаутом и лимитом тела.
func fetchUpdateStatus(ctx context.Context) (fiber.Map, bool) {
	agent := strings.TrimRight(platform.GetEnv("HOST_AGENT_URL", "http://host.containers.internal:9998"), "/")
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, agent+"/update/status", nil)
	if err != nil {
		return nil, false
	}
	if t := adminToken(); t != "" {
		req.Header.Set("X-Admin-Token", t)
	}
	resp, err := hostAgentClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var out fiber.Map
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return nil, false
	}
	return out, true
}

func main() {
	if err := platform.ValidateEnv([]string{"POSTGRES_URL"}); err != nil {
		log.Fatalf("❌ %v", err)
	}
	rdb = redis.NewClient(&redis.Options{
		Addr: platform.GetEnv("REDIS_ADDR", "localhost:6379"),
	})

	// Подключение к PostgreSQL (для метрик здоровья сервисов)
	pgURL := platform.GetEnv("POSTGRES_URL", "postgres://nas:nas_secret@postgres:5432/nas_history?sslmode=disable")
	var err error
	db, err = pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Printf("⚠️ PostgreSQL: %v (healthcheck будет без БД)", err)
		db = nil
	} else if err := migrateHealth(); err != nil {
		log.Printf("⚠️ Миграция service_health: %v", err)
	}
	if db != nil {
		if err := migrateMemory(); err != nil {
			log.Printf("⚠️ Миграция memory_checks: %v", err)
		}
	}
	if db != nil {
		defer db.Close()
	}

	// Запускаем все задачи в фоне
	for _, t := range tasks {
		go runTask(t)
	}

	app := newFiber()
	platform.StartPprof()
	app.Use(metricsMiddleware())
	registerRoutes(app)

	log.Println("⏰ scheduler-service на :8084")
	if adminToken() == "" {
		log.Println("⚠️ ADMIN_TOKEN не задан — мутирующие ручки будут отдавать 403")
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		if err := app.ShutdownWithTimeout(15 * time.Second); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	if err := app.Listen(":8084"); err != nil {
		// Без log.Fatal: defer db.Close() должен выполниться.
		log.Printf("listen: %v", err)
	}
}

// newFiber — единая конфигурация для прода и тестов.
func newFiber() *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:    1 << 20,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	})
	// Без recover любая паника в хендлере роняет весь планировщик.
	app.Use(fiberrecover.New())
	return app
}

// registerRoutes — единственное место регистрации маршрутов: прод и тесты
// используют один и тот же роутер (иначе auth-регрессии не ловятся).
func registerRoutes(app *fiber.App) {
	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("scheduler", nil)) })

	app.Get("/tasks", func(c fiber.Ctx) error {
		result := make([]fiber.Map, 0, len(tasks))
		for _, t := range tasks {
			result = append(result, fiber.Map{
				"name":     t.Name,
				"schedule": t.Schedule.String(),
			})
		}
		return c.JSON(result)
	})

	app.Post("/run/:name", requireAdminToken, func(c fiber.Ctx) error {
		name := c.Params("name")
		for _, t := range tasks {
			if t.Name == name {
				if err := startTask(t); err != nil {
					return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error(), "task": name})
				}
				return c.JSON(fiber.Map{"status": "started", "task": name})
			}
		}
		return c.Status(404).JSON(fiber.Map{"error": "task not found"})
	})

	// Ручной запуск бэкапа Gitea. Сама задача защищена от параллельного запуска.
	app.Post("/gitea/backup", requireAdminToken, func(c fiber.Ctx) error {
		if err := startTask(Task{Name: "gitea-backup", Schedule: 24 * time.Hour}); err != nil {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error(), "task": "gitea-backup"})
		}
		return c.JSON(fiber.Map{"status": "started", "task": "gitea-backup"})
	})

	// ---- Health сервисов ----
	app.Get("/health/services", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		qctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		rows, err := db.Query(qctx, `
			SELECT DISTINCT ON (service) service, status, COALESCE(response_ms,0), COALESCE(error,''), checked_at
			FROM service_health
			ORDER BY service, checked_at DESC`)
		if err != nil {
			log.Printf("health query: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "db query failed"})
		}
		defer rows.Close()
		services := make([]fiber.Map, 0, 16)
		up, down := 0, 0
		for rows.Next() {
			var name, status, errText string
			var ms int
			var at time.Time
			if err := rows.Scan(&name, &status, &ms, &errText, &at); err != nil {
				log.Printf("health/services scan: %v", err)
				continue
			}
			if status == "up" {
				up++
			} else {
				down++
			}
			services = append(services, fiber.Map{
				"name": name, "status": status, "response_ms": ms, "error": errText, "last_check": at,
			})
		}
		if err := rows.Err(); err != nil {
			log.Printf("health/services rows: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "db read failed"})
		}
		return c.JSON(fiber.Map{
			"services": services,
			"summary":  fiber.Map{"up": up, "down": down, "total": len(services)},
		})
	})

	app.Get("/health/summary", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		var up, down int
		qctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		if err := db.QueryRow(qctx, `
			SELECT
				COUNT(*) FILTER (WHERE status='up'),
				COUNT(*) FILTER (WHERE status<>'up')
			FROM (SELECT DISTINCT ON (service) service, status FROM service_health ORDER BY service, checked_at DESC) t`).Scan(&up, &down); err != nil {
			log.Printf("health/summary: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "db query failed"})
		}
		return c.JSON(fiber.Map{"up": up, "down": down, "total": up + down})
	})

	app.Get("/health/history", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		service := c.Query("service")
		limit, _ := strconv.Atoi(c.Query("limit", "100"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		qctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		rows, err := db.Query(qctx, `
			SELECT service, status, COALESCE(response_ms,0), COALESCE(error,''), checked_at
			FROM service_health WHERE service=$1 ORDER BY checked_at DESC LIMIT $2`, service, limit)
		if err != nil {
			log.Printf("health query: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "db query failed"})
		}
		defer rows.Close()
		items := make([]fiber.Map, 0, 16)
		for rows.Next() {
			var name, status, errText string
			var ms int
			var at time.Time
			if err := rows.Scan(&name, &status, &ms, &errText, &at); err != nil {
				log.Printf("health/history scan: %v", err)
				continue
			}
			items = append(items, fiber.Map{"name": name, "status": status, "response_ms": ms, "error": errText, "checked_at": at})
		}
		if err := rows.Err(); err != nil {
			log.Printf("health/history rows: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "db read failed"})
		}
		return c.JSON(fiber.Map{"service": service, "history": items, "count": len(items)})
	})

	// GET /health/memory — последние метрики памяти/горутин по сервисам.
	app.Get("/health/memory", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		reqCtx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()

		// Один запрос вместо N+1: предыдущий замер берём LATERAL-джойном.
		rows, err := db.Query(reqCtx, `
			WITH cur AS (
				SELECT DISTINCT ON (service) service,
				       COALESCE(mem_mb,0) AS mem_mb,
				       COALESCE(goroutines,0) AS goroutines,
				       COALESCE(gc_count,0) AS gc_count,
				       checked_at
				FROM memory_checks
				ORDER BY service, checked_at DESC
			)
			SELECT cur.service, cur.mem_mb, cur.goroutines, cur.gc_count, cur.checked_at,
			       COALESCE(prev.mem_mb,0), COALESCE(prev.goroutines,0)
			FROM cur
			LEFT JOIN LATERAL (
				SELECT m.mem_mb, m.goroutines
				FROM memory_checks m
				WHERE m.service = cur.service
				  AND m.checked_at < NOW() - INTERVAL '4 minutes'
				ORDER BY m.checked_at DESC
				LIMIT 1
			) prev ON true
			ORDER BY cur.service`)
		if err != nil {
			log.Printf("health/memory query: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "db query failed"})
		}
		defer rows.Close()

		items := make([]fiber.Map, 0, 16)
		alerts := make([]fiber.Map, 0, 4)
		for rows.Next() {
			var name string
			var mem, gor, gc, prevMem, prevGor int
			var at time.Time
			if err := rows.Scan(&name, &mem, &gor, &gc, &at, &prevMem, &prevGor); err != nil {
				log.Printf("health/memory scan: %v", err)
				continue
			}
			items = append(items, fiber.Map{"service": name, "mem_mb": mem, "goroutines": gor, "gc_count": gc, "checked_at": at})
			if prevGor > 0 && gor-prevGor >= 20 {
				alerts = append(alerts, fiber.Map{"service": name, "type": "goroutines", "prev": prevGor, "now": gor})
			}
			if prevMem > 0 && mem > prevMem*3/2 && mem-prevMem >= 30 {
				alerts = append(alerts, fiber.Map{"service": name, "type": "memory", "prev_mb": prevMem, "now_mb": mem})
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("health/memory rows: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "db read failed"})
		}
		return c.JSON(fiber.Map{"services": items, "alerts": alerts})
	})

	// ---- Обновление стека (статус из Redis, пишет scripts/update-stack.sh) ----
	app.Get("/update/status", func(c fiber.Ctx) error {
		const ttl = 30 * time.Second

		updateStatusCache.mu.Lock()
		if time.Since(updateStatusCache.at) < ttl && updateStatusCache.body != nil {
			body := updateStatusCache.body
			updateStatusCache.mu.Unlock()
			return c.JSON(body)
		}
		updateStatusCache.mu.Unlock()

		// Статус обновлений берём у host-agent (на хосте есть git).
		out, ok := fetchUpdateStatus(c.Context())
		if !ok {
			// Fallback: метаданные из Redis.
			last, status, commit := "", "", ""
			if rdb != nil {
				last, _ = rdb.Get(ctx, "update:last").Result()
				status, _ = rdb.Get(ctx, "update:status").Result()
				commit, _ = rdb.Get(ctx, "update:commit").Result()
			}
			// nil = «неизвестно», а не «обновлений нет».
			out = fiber.Map{"last": last, "status": status, "commit": commit, "updates_available": nil, "known": false}
		}
		// Кэшируем и успех, и fallback: иначе при лежащем host-agent каждый
		// запрос ждёт 8 секунд.
		updateStatusCache.mu.Lock()
		updateStatusCache.at = time.Now()
		updateStatusCache.body = out
		updateStatusCache.mu.Unlock()
		return c.JSON(out)
	})
	app.Post("/update/run", requireAdminToken, func(c fiber.Ctx) error {
		agent := strings.TrimRight(platform.GetEnv("HOST_AGENT_URL", "http://host.containers.internal:9998"), "/")
		req, err := http.NewRequestWithContext(c.Context(), http.MethodPost, agent+"/update/run", nil)
		if err != nil {
			log.Printf("update/run: bad HOST_AGENT_URL: %v", err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "host-agent misconfigured"})
		}
		// host-agent свои ручки защищает тем же ADMIN_TOKEN — разметка
		// обязательна, иначе /update/run вернёт 403.
		if t := adminToken(); t != "" {
			req.Header.Set("X-Admin-Token", t)
		}
		resp, err := hostAgentClient.Do(req)
		if err != nil {
			log.Printf("update/run: host-agent: %v", err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "host-agent unavailable"})
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
		}()
		switch {
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
			log.Printf("⛔ host-agent отказал в /update/run (проверь ADMIN_TOKEN)")
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "host-agent rejected admin token"})
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			log.Printf("update/run: неожиданный редирект от host-agent (%d)", resp.StatusCode)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "unexpected redirect from host-agent"})
		case resp.StatusCode < 200 || resp.StatusCode > 299:
			log.Printf("update/run: host-agent HTTP %d", resp.StatusCode)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "host-agent error"})
		}
		return c.JSON(fiber.Map{"status": "started"})
	})

	// ---- Prowlarr auto-setup (добавляет недостающие публичные индексаторы) ----
	app.Post("/prowlarr/setup", requireAdminToken, func(c fiber.Ctx) error {
		var cfgReq struct {
			APIKey string `json:"api_key"`
		}
		_ = c.Bind().JSON(&cfgReq)

		// URL — только из env. Раньше поле url из тела подменяло адрес и
		// уводило PROWLARR_API_KEY на произвольный хост (SSRF + эксфильтрация).
		// Ключ из тела допустим только для авторизованного админа (setup wizard
		// передаёт свежий ключ, которого ещё нет в env планировщика).
		base, err := prowlarrBaseURL()
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		apiKey := strings.TrimSpace(cfgReq.APIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(platform.GetEnv("PROWLARR_API_KEY", ""))
		}
		if apiKey == "" {
			return c.Status(400).JSON(fiber.Map{"error": "PROWLARR_API_KEY not set"})
		}
		targets := []string{"1337x", "The Pirate Bay", "Byrutor"}
		hc := prowlarrClient

		getJSON := func(path string, out any) error {
			req, err := http.NewRequestWithContext(c.Context(), http.MethodGet, base+path, nil)
			if err != nil {
				return fmt.Errorf("bad PROWLARR_URL")
			}
			req.Header.Set("X-Api-Key", apiKey)
			resp, err := hc.Do(req)
			if err != nil {
				return err
			}
			defer func() {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
			}()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("HTTP %d", resp.StatusCode) // тело апстрима — только в лог
			}
			return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
		}

		var current []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}
		if err := getJSON("/api/v1/indexer", &current); err != nil {
			log.Printf("prowlarr indexer: %v", err)
			return c.Status(502).JSON(fiber.Map{"error": "prowlarr unavailable"})
		}
		have := map[string]bool{}
		for _, i := range current {
			have[strings.ToLower(i.Name)] = true
		}

		var schema []map[string]any
		if err := getJSON("/api/v1/indexer/schema", &schema); err != nil {
			log.Printf("prowlarr schema: %v", err)
			return c.Status(502).JSON(fiber.Map{"error": "prowlarr schema unavailable"})
		}
		byName := map[string]map[string]any{}
		for _, s := range schema {
			if n, ok := s["name"].(string); ok {
				byName[strings.ToLower(n)] = s
			}
		}

		appProfile := 0
		var profiles []struct {
			ID int `json:"id"`
		}
		if err := getJSON("/api/v1/appprofile", &profiles); err == nil && len(profiles) > 0 {
			appProfile = profiles[0].ID
		}

		added, skipped, errs := []string{}, []string{}, []string{}
		for _, name := range targets {
			if have[strings.ToLower(name)] {
				skipped = append(skipped, name)
				continue
			}
			def, ok := byName[strings.ToLower(name)]
			if !ok {
				errs = append(errs, name+": нет в схеме Prowlarr")
				continue
			}
			body := map[string]any{}
			for k, v := range def {
				body[k] = v
			}
			body["name"] = name
			body["enable"] = true
			if appProfile > 0 {
				body["appProfileId"] = appProfile
			}
			if _, ok := body["tags"]; !ok {
				body["tags"] = []int{}
			}
			b, err := json.Marshal(body)
			if err != nil {
				errs = append(errs, name+": encode")
				continue
			}
			req, err := http.NewRequestWithContext(c.Context(), http.MethodPost, base+"/api/v1/indexer", bytes.NewReader(b))
			if err != nil {
				errs = append(errs, name+": bad request")
				continue
			}
			req.Header.Set("X-Api-Key", apiKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := hc.Do(req)
			if err != nil {
				log.Printf("prowlarr setup %s: %v", name, err)
				errs = append(errs, name+": request failed")
				continue
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				added = append(added, name)
			} else {
				rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				log.Printf("prowlarr setup %s: HTTP %d: %s", name, resp.StatusCode, truncateLog(string(rb), 300))
				errs = append(errs, fmt.Sprintf("%s: HTTP %d", name, resp.StatusCode))
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
		}
		return c.JSON(fiber.Map{"status": "ok", "added": added, "skipped": skipped, "errors": errs})
	})

	// ---- Бэкапы ----
	app.Get("/backup/list", func(c fiber.Ctx) error {
		return c.JSON(listBackups())
	})
	app.Post("/backup/run", requireAdminToken, func(c fiber.Ctx) error {
		if !pgBackupMu.TryLock() {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "dump/restore уже выполняется"})
		}
		go func() {
			defer pgBackupMu.Unlock()
			if err := runPgBackupLocked(); err != nil {
				log.Printf("❌ backup/run: %v", err)
			}
		}()
		return c.JSON(fiber.Map{"status": "started"})
	})
	app.Post("/backup/restore", requireAdminToken, func(c fiber.Ctx) error {
		var b struct {
			File    string `json:"file"`
			Confirm bool   `json:"confirm"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.File == "" {
			return c.Status(400).JSON(fiber.Map{"error": "нужен file"})
		}
		if err := restoreBackup(b.File, b.Confirm); err != nil {
			// stderr pg_restore/psql может содержать имена объектов и DSN.
			log.Printf("restore %q: %v", b.File, err)
			return c.Status(400).JSON(fiber.Map{"error": "restore failed"})
		}
		log.Printf("♻️ БД восстановлена из %s", b.File)
		return c.JSON(fiber.Map{"status": "restored", "file": b.File})
	})

}

func runTask(t Task) {
	time.Sleep(30 * time.Second)
	ticker := time.NewTicker(t.Schedule)
	defer ticker.Stop()
	for {
		if err := execute(t); err != nil && !errors.Is(err, errTaskBusy) {
			log.Printf("⚠️ %s: %v", t.Name, err)
		}
		<-ticker.C
	}
}

var taskLocks sync.Map // name -> *sync.Mutex

func taskLock(name string) *sync.Mutex {
	v, _ := taskLocks.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

var errTaskBusy = errors.New("task already running")

// execute запускает задачу синхронно и сообщает, была ли она уже занята.
func execute(t Task) error {
	mu := taskLock(t.Name)
	if !mu.TryLock() {
		log.Printf("⏭️  %s уже выполняется — пропуск", t.Name)
		return errTaskBusy
	}
	defer mu.Unlock()
	executeLocked(t)
	return nil
}

// startTask запускает задачу асинхронно и сразу сообщает, принята ли она.
func startTask(t Task) error {
	mu := taskLock(t.Name)
	if !mu.TryLock() {
		return errTaskBusy
	}
	go func() {
		defer mu.Unlock()
		executeLocked(t)
	}()
	return nil
}

var taskHandlers = map[string]func() error{
	"pg-backup":              runPgBackup,
	"wishlist-release-check": runWishlistCheck,
	"series-check":           runSeriesCheck,
	"healthcheck-services":   runHealthcheck,
	"disk-check-detailed":    runDiskCheckDetailed,
	"memory-check":           runMemoryCheck,
	"oom-check":              runOomCheck,
	"gitea-backup":           runGiteaBackup,
	"metrics-retention":      runRetention,
}

func executeLocked(t Task) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("🔥 panic in %s: %v", t.Name, r)
			setTaskResult(t.Name, fmt.Sprintf("panic: %v", r))
		}
	}()

	log.Printf("▶️  Запускаю: %s", t.Name)
	start := time.Now()

	status := "success"
	if run, ok := taskHandlers[t.Name]; ok {
		if err := run(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ %s: %v", t.Name, err)
		} else {
			log.Printf("✅ %s завершён", t.Name)
		}
	} else if t.Command != "" {
		runCtx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()

		var out cappedBuffer
		out.max = 64 << 10
		cmd := exec.CommandContext(runCtx, t.Command, t.Args...)
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ %s: %v", t.Name, err)
		}
		if out.buf.Len() > 0 {
			log.Printf("   Вывод: %s", truncateLog(out.buf.String(), 2000))
		}
	} else {
		status = "error: no runner"
		log.Printf("❌ %s: no runner", t.Name)
	}

	duration := time.Since(start)
	log.Printf("🏁 %s завершён за %v: %s", t.Name, duration, status)
	setTaskResult(t.Name, status)
}

func setTaskResult(name, status string) {
	if rdb == nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Set(rctx, "task:"+name+":last", time.Now().Format(time.RFC3339), 7*24*time.Hour).Err(); err != nil {
		log.Printf("redis: task last %s: %v", name, err)
	}
	if err := rdb.Set(rctx, "task:"+name+":status", status, 7*24*time.Hour).Err(); err != nil {
		log.Printf("redis: task status %s: %v", name, err)
	}
}

// cappedBuffer ограничивает объём вывода дочернего процесса в памяти.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remain := b.max - b.buf.Len(); remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = b.buf.Write(p)
	}
	return n, nil
}

var giteaBackupMu sync.Mutex

var (
	sshHostRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,253}$`)
	sshUserRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	sshScriptRe = regexp.MustCompile(`^/[A-Za-z0-9._/@+-]{1,1000}$`)
)

// validateSSHParams не даёт подставить ssh-опции или shell-метасимволы
// через переменные окружения.
func validateSSHParams(host, user, port, script string) error {
	if !sshHostRe.MatchString(host) || strings.HasPrefix(host, "-") {
		return fmt.Errorf("NAS_HOST: недопустимые символы")
	}
	if !sshUserRe.MatchString(user) || strings.HasPrefix(user, "-") {
		return fmt.Errorf("NAS_USER: недопустимые символы")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("NAS_SSH_PORT: ожидается 1..65535")
	}
	if !sshScriptRe.MatchString(script) || strings.Contains(script, "..") {
		return fmt.Errorf("NAS_BACKUP_SCRIPT: ожидается абсолютный путь без '..'")
	}
	return nil
}

// sshChildEnv не наследует ADMIN_TOKEN/POSTGRES_URL/NAS_PASS: они не должны
// попадать в /proc/<pid>/environ дочернего ssh/sshpass.
func sshChildEnv(extra ...string) []string {
	env := make([]string, 0, 6+len(extra))
	for _, k := range []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ", "SSH_AUTH_SOCK"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if _, ok := os.LookupEnv("PATH"); !ok {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return append(env, extra...)
}

// giteaBackupCommand builds the SSH command for the NAS backup script.
// Key authentication is preferred; NAS_PASS is an optional fallback and is
// passed through SSHPASS (-e) so it does not appear in the process list.
func giteaBackupCommand(ctx context.Context) (*exec.Cmd, error) {
	host := strings.TrimSpace(platform.GetEnv("NAS_HOST", "192.168.0.5"))
	user := strings.TrimSpace(platform.GetEnv("NAS_USER", "FuzzyHD"))
	port := strings.TrimSpace(platform.GetEnv("NAS_SSH_PORT", "5022"))
	script := strings.TrimSpace(platform.GetEnv("NAS_BACKUP_SCRIPT", "/volume1/docker/gitea-backups/backup-gitea.sh"))
	if err := validateSSHParams(host, user, port, script); err != nil {
		return nil, err
	}

	knownHosts := strings.TrimSpace(platform.GetEnv("NAS_KNOWN_HOSTS", "/root/.ssh/known_hosts"))
	// accept-new — TOFU при первом подключении; yes требует заранее
	// залитый known_hosts (volume /root/.ssh).
	strict := strings.TrimSpace(platform.GetEnv("NAS_STRICT_HOST_KEY", "accept-new"))
	switch strict {
	case "yes", "accept-new", "no":
	default:
		strict = "accept-new"
	}

	sshArgs := []string{
		"-p", port,
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=" + strict,
		"-o", "UserKnownHostsFile=" + knownHosts,
	}

	keyPath := platform.GetEnv("NAS_SSH_KEY", "/root/.ssh/id_ed25519")
	if info, err := os.Stat(keyPath); err == nil && !info.IsDir() {
		args := append([]string{}, sshArgs...)
		args = append(args,
			"-i", keyPath,
			"-o", "IdentitiesOnly=yes",
			"-o", "BatchMode=yes",
			"--", user+"@"+host, "sh", script,
		)
		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.Env = sshChildEnv()
		return cmd, nil
	}

	password := os.Getenv("NAS_PASS")
	if password == "" {
		return nil, fmt.Errorf("SSH key %q не найден, а NAS_PASS пуст", keyPath)
	}
	args := append([]string{"-e", "ssh"}, sshArgs...)
	args = append(args, "--", user+"@"+host, "sh", script)
	cmd := exec.CommandContext(ctx, "sshpass", args...)
	cmd.Env = sshChildEnv("SSHPASS=" + password)
	return cmd, nil
}

func runGiteaBackup() error {
	if !giteaBackupMu.TryLock() {
		return fmt.Errorf("бэкап Gitea уже выполняется")
	}
	defer giteaBackupMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cmd, err := giteaBackupCommand(ctx)
	if err != nil {
		return err
	}
	var out cappedBuffer
	out.max = 64 << 10
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gitea backup failed: %w: %s", err, truncateLog(out.buf.String(), 400))
	}
	if s := strings.TrimSpace(out.buf.String()); s != "" {
		log.Printf("   Gitea backup: %s", truncateLog(s, 2000))
	}
	return nil
}

// pgBackupMu сериализует dump/restore: параллельные прогоны не должны
// писать в один файл и восстанавливать БД во время дампа.
var pgBackupMu sync.Mutex

// runPgBackup делает дамп PostgreSQL в daily/weekly/monthly с ротацией.
func runPgBackup() error {
	if !pgBackupMu.TryLock() {
		return fmt.Errorf("dump/restore уже выполняется")
	}
	defer pgBackupMu.Unlock()
	return runPgBackupLocked()
}

// runPgBackupLocked — тело дампа; вызывающий уже держит pgBackupMu.
func runPgBackupLocked() error {
	pgURL := strings.TrimSpace(platform.GetEnv("POSTGRES_URL", ""))
	if pgURL == "" {
		return fmt.Errorf("POSTGRES_URL не задан")
	}
	baseDir := platform.GetEnv("BACKUP_DIR", "/backups/postgres")

	dailyDir := filepath.Join(baseDir, "daily")
	weeklyDir := filepath.Join(baseDir, "weekly")
	monthlyDir := filepath.Join(baseDir, "monthly")
	for _, d := range []string{dailyDir, weeklyDir, monthlyDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}

	now := time.Now()

	dailyFile := filepath.Join(dailyDir, "nas_history-"+now.Format("20060102-150405.000000000")+".sql.gz")
	if err := dumpPG(pgURL, dailyFile); err != nil {
		return err
	}
	rotateDir(dailyDir, envInt("BACKUP_KEEP", 7))

	if now.Weekday() == time.Sunday {
		isoYear, isoWeek := now.ISOWeek()
		week := fmt.Sprintf("%d-W%02d", isoYear, isoWeek)
		if err := dumpPG(pgURL, filepath.Join(weeklyDir, "nas_history-"+week+".sql.gz")); err != nil {
			log.Printf("⚠️ weekly backup failed: %v", err)
		}
		rotateDir(weeklyDir, envInt("BACKUP_KEEP_WEEKLY", 4))
	}
	if now.Day() == 1 {
		month := now.Format("2006-01")
		if err := dumpPG(pgURL, filepath.Join(monthlyDir, "nas_history-"+month+".sql.gz")); err != nil {
			log.Printf("⚠️ monthly backup failed: %v", err)
		}
		rotateDir(monthlyDir, envInt("BACKUP_KEEP_MONTHLY", 12))
	}

	if rdb != nil {
		_ = rdb.Publish(context.Background(), "backup.completed", filepath.Base(dailyFile)).Err()
	}
	return nil
}

// pgDSN отделяет пароль от DSN: пароль уходит в PGPASSWORD, а не в argv
// (иначе виден в /proc/<pid>/cmdline).
func pgDSN(raw string) (dsn, password string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", err
	}
	if u.Scheme == "" {
		return raw, "", nil // key=value DSN — пароль не выделить, оставляем как есть
	}
	if u.User != nil {
		if p, ok := u.User.Password(); ok {
			password = p
			u.User = url.User(u.User.Username())
		}
	}
	return u.String(), password, nil
}

func pgEnv(password string) []string {
	// Не наследуем os.Environ(): в контейнере лежат ADMIN_TOKEN,
	// PROWLARR_API_KEY и POSTGRES_URL с паролем.
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"PGCONNECT_TIMEOUT=10",
		"PGAPPNAME=scheduler-service",
	}
	if password != "" {
		env = append(env, "PGPASSWORD="+password)
	}
	return env
}

func envInt(key string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(platform.GetEnv(key, "")))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// truncateLog приводит stderr к одной строке и режет длину для логов/JSON.
func truncateLog(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 || r == 0x1b {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(strings.TrimSpace(s), "?"))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func dumpPG(pgURL, out string) error {
	dsn, password, err := pgDSN(pgURL)
	if err != nil {
		return fmt.Errorf("POSTGRES_URL: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Пишем в temp и переименовываем только после успешного gzip-финала:
	// убитый посреди дампа процесс не оставит «валидный» обрезанный файл.
	f, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	gz := gzip.NewWriter(f)

	cmd := exec.CommandContext(ctx, "pg_dump",
		"--no-owner", "--no-privileges", "--dbname", dsn)
	cmd.Env = pgEnv(password)
	cmd.Stdout = gz
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	gzErr := gz.Close()
	fErr := f.Close()

	fail := func(e error) error {
		return fmt.Errorf("%w: %s", e, truncateLog(stderr.String(), 400))
	}
	if runErr != nil {
		return fail(runErr)
	}
	if gzErr != nil {
		return fail(gzErr)
	}
	if fErr != nil {
		return fail(fErr)
	}
	fi, err := os.Stat(tmpName)
	if err != nil || fi.Size() < 32 { // пустой gzip ≈ 20 байт
		return errNoBackup
	}
	if err := os.Rename(tmpName, out); err != nil {
		return err
	}
	committed = true
	return nil
}

func rotateDir(dir string, keep int) {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		old := names[0]
		names = names[1:]
		_ = os.Remove(filepath.Join(dir, old))
		log.Printf("🗑️ Удалён старый бэкап: %s", old)
	}
}

// listBackups собирает список файлов по типам.
func listBackups() fiber.Map {
	baseDir := platform.GetEnv("BACKUP_DIR", "/backups/postgres")
	out := fiber.Map{}
	for _, kind := range []string{"daily", "weekly", "monthly"} {
		dir := filepath.Join(baseDir, kind)
		arr := []fiber.Map{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !backupNameRe.MatchString(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			arr = append(arr, fiber.Map{
				"file":       filepath.ToSlash(filepath.Join(kind, e.Name())),
				"name":       e.Name(),
				"size_bytes": info.Size(),
				"created_at": info.ModTime(),
			})
		}
		sort.SliceStable(arr, func(i, j int) bool { return arr[i]["name"].(string) > arr[j]["name"].(string) })
		out[kind] = arr
	}
	return out
}

var backupNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.sql\.gz$`)

// safeBackupPath проверяет, что файл лежит внутри BACKUP_DIR, не является
// симлинком наружу и имеет допустимое имя (без shell-метасимволов).
func safeBackupPath(file string) (string, error) {
	baseDir, err := filepath.Abs(platform.GetEnv("BACKUP_DIR", "/backups/postgres"))
	if err != nil {
		return "", fmt.Errorf("bad BACKUP_DIR")
	}
	baseDir, err = filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", fmt.Errorf("BACKUP_DIR недоступен")
	}
	file = strings.TrimSpace(file)
	if file == "" {
		return "", fmt.Errorf("пустое имя файла")
	}
	// API отдаёт относительные имена (daily/x.sql.gz); абсолютные пути
	// продолжаем принимать для совместимости.
	if !filepath.IsAbs(file) {
		file = filepath.Join(baseDir, file)
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", fmt.Errorf("недопустимый путь")
	}
	rel, err := filepath.Rel(baseDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("путь вне каталога бэкапов")
	}
	if !backupNameRe.MatchString(filepath.Base(abs)) {
		return "", fmt.Errorf("недопустимое имя файла бэкапа")
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("файл не найден")
	}
	relReal, err := filepath.Rel(baseDir, real)
	if err != nil || relReal == ".." || strings.HasPrefix(relReal, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("симлинк вне каталога бэкапов")
	}
	fi, err := os.Lstat(real)
	if err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("файл не найден")
	}
	return real, nil
}

// restoreBackup восстанавливает БД из gzip-дампа без sh -c.
func restoreBackup(file string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("нужно confirm=true")
	}
	clean, err := safeBackupPath(file)
	if err != nil {
		return err
	}
	if !pgBackupMu.TryLock() {
		return fmt.Errorf("dump/restore уже выполняется")
	}
	defer pgBackupMu.Unlock()

	pgURL := strings.TrimSpace(platform.GetEnv("POSTGRES_URL", ""))
	if pgURL == "" {
		return fmt.Errorf("POSTGRES_URL не задан")
	}
	dsn, password, err := pgDSN(pgURL)
	if err != nil {
		return fmt.Errorf("POSTGRES_URL: %w", err)
	}

	f, err := os.Open(clean)
	if err != nil {
		return fmt.Errorf("файл не найден")
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("не gzip-дамп: %w", err)
	}
	defer func() { _ = gz.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "psql",
		"--dbname", dsn, "--no-psqlrc", "--quiet",
		"--set", "ON_ERROR_STOP=1", "--single-transaction")
	cmd.Env = pgEnv(password)
	cmd.Stdin = gz
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restore: %w: %s", err, truncateLog(stderr.String(), 400))
	}
	return nil
}

var errNoBackup = fmt.Errorf("pg_dump не создал файл (проверь pg_dump и POSTGRES_URL)")

// runWishlistCheck запрашивает у media-service проверку релизов wishlist.
func runWishlistCheck() error {
	base := platform.GetEnv("MEDIA_SERVICE_URL", "http://media-service:8091")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(base + "/wishlist/check")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("media-service status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var res struct {
		Count    int `json:"count"`
		Released []struct {
			Title string `json:"title"`
		} `json:"released"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	if res.Count > 0 {
		log.Printf("🎬 Wishlist: вышло %d релиз(ов)", res.Count)
	}
	return nil
}

// runSeriesCheck проверяет подписки на сериалы и добавляет новые серии.
func runSeriesCheck() error {
	base := platform.GetEnv("MEDIA_SERVICE_URL", "http://media-service:8091")
	client := &http.Client{Timeout: 120 * time.Second}

	resp, err := client.Get(base + "/subscriptions")
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	var subs struct {
		Items []struct {
			ID      int  `json:"id"`
			Enabled bool `json:"enabled"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &subs); err != nil {
		return err
	}
	total := 0
	for _, s := range subs.Items {
		if !s.Enabled {
			continue
		}
		r2, err := client.Post(base+"/subscriptions/"+strconv.Itoa(s.ID)+"/check", "application/json", nil)
		if err != nil {
			continue
		}
		b2, _ := io.ReadAll(io.LimitReader(r2.Body, 8<<20))
		r2.Body.Close()
		var chk struct {
			Count int `json:"count"`
		}
		if json.Unmarshal(b2, &chk) == nil {
			total += chk.Count
		}
	}
	if total > 0 {
		log.Printf("📺 Сериалы: найдено %d новых серий", total)
	}
	return nil
}

// ============================================================
// HEALTHCHECK СЕРВИСОВ
// ============================================================

var healthTargets = []struct {
	Name string
	URL  string
}{
	{"gateway", "http://gateway:8080/health"},
	{"torrent-service", "http://torrent-service:8081/health"},
	{"stats-service", "http://stats-service:8082/health"},
	{"scheduler-service", "http://scheduler-service:8084/health"},
	{"history-service", "http://history-service:8085/health"},
	{"auth-service", "http://auth-service:8086/health"},
	{"books-service", "http://books-service:8087/health"},
	{"search-service", "http://search-service:8088/health"},
	{"youtube-service", "http://youtube-service:8089/health"},
	{"web-dashboard", "http://web-dashboard:8090/health"},
	{"media-service", "http://media-service:8091/health"},
	{"music-service", "http://music-service:8092/health"},
	{"notify-service", "http://host.containers.internal:8083/health"},
}

func migrateHealth() error {
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS service_health (
			id SERIAL PRIMARY KEY,
			service VARCHAR(64) NOT NULL,
			status VARCHAR(16) NOT NULL,
			response_ms INT,
			error TEXT,
			checked_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_service_health
			ON service_health(service, checked_at DESC);
	`)
	return err
}

func migrateMemory() error {
	_, err := db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS memory_checks (
			id SERIAL PRIMARY KEY,
			service VARCHAR(64) NOT NULL,
			mem_mb INT,
			goroutines INT,
			gc_count INT,
			checked_at TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_memory_checks
			ON memory_checks(service, checked_at DESC);
	`)
	return err
}

// runMemoryCheck снимает mem_mb/goroutines каждого сервиса, пишет в БД и
// алертит при росте горутин (>20) или памяти (>50%) за 5 минут.
func runMemoryCheck() error {
	notifyURL := platform.GetEnv("NOTIFY_SERVICE_URL", "http://host.containers.internal:8083")
	client := &http.Client{Timeout: 5 * time.Second}

	for _, t := range healthTargets {
		resp, err := client.Get(t.URL)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		var h struct {
			MemMB      int `json:"mem_mb"`
			Goroutines int `json:"goroutines"`
			GCCount    int `json:"gc_count"`
		}
		if json.Unmarshal(body, &h) != nil {
			continue
		}

		if db != nil {
			var prevMem, prevGor int
			_ = db.QueryRow(ctx, `
				SELECT COALESCE(mem_mb,0), COALESCE(goroutines,0) FROM memory_checks
				WHERE service=$1 AND checked_at < NOW() - INTERVAL '4 minutes'
				ORDER BY checked_at DESC LIMIT 1`, t.Name).Scan(&prevMem, &prevGor)

			_, _ = db.Exec(ctx, `
				INSERT INTO memory_checks (service, mem_mb, goroutines, gc_count)
				VALUES ($1,$2,$3,$4)`, t.Name, h.MemMB, h.Goroutines, h.GCCount)

			if prevGor > 0 && h.Goroutines-prevGor >= 20 {
				msg := fmt.Sprintf("⚠️ %s: goroutines %d → %d", t.Name, prevGor, h.Goroutines)
				log.Printf("%s", msg)
				sendNotify(notifyURL, msg, "warning")
			}
			if prevMem > 0 && h.MemMB > prevMem*3/2 && h.MemMB-prevMem >= 30 {
				msg := fmt.Sprintf("⚠️ %s: память %dMB → %dMB", t.Name, prevMem, h.MemMB)
				log.Printf("%s", msg)
				sendNotify(notifyURL, msg, "warning")
			}
		}
	}
	return nil
}

// runHealthcheck пингует /health всех сервисов, пишет в БД и шлёт алерты
// при падении/восстановлении (после 2 подряд неудач).
func runHealthcheck() error {
	client := &http.Client{Timeout: 5 * time.Second}
	notifyURL := platform.GetEnv("NOTIFY_SERVICE_URL", "http://host.containers.internal:8083")

	for _, t := range healthTargets {
		start := time.Now()
		resp, err := client.Get(t.URL)
		responseMs := int(time.Since(start).Milliseconds())

		status := "down"
		errText := ""
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				status = "up"
			} else {
				errText = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			// Дочитываем тело: иначе соединение не переиспользуется.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
		} else {
			errText = err.Error()
		}

		if db != nil {
			_, _ = db.Exec(ctx, `
				INSERT INTO service_health (service, status, response_ms, error)
				VALUES ($1,$2,$3,$4)`, t.Name, status, responseMs, errText)
		}

		if rdb == nil {
			continue
		}
		key := "health:fails:" + t.Name
		alertKey := "health:alerted:" + t.Name

		if status == "down" {
			count, err := rdb.Incr(ctx, key).Result()
			if err != nil {
				log.Printf("health: redis incr %s: %v", t.Name, err)
				continue
			}
			_ = rdb.Expire(ctx, key, 5*time.Minute).Err()
			if count < 2 {
				continue
			}
			// Один алерт на инцидент; повтор — не чаще раза в 6 часов.
			fresh, err := rdb.SetNX(ctx, alertKey, "1", 6*time.Hour).Result()
			if err != nil {
				log.Printf("health: redis setnx %s: %v", t.Name, err)
				continue
			}
			if fresh {
				b, _ := json.Marshal(fiber.Map{"name": t.Name, "error": errText})
				_ = rdb.Publish(ctx, "service.down", string(b)).Err()
				sendNotify(notifyURL, "❌ "+t.Name+" down: "+truncateLog(errText, 300), "error")
				log.Printf("❌ %s down: %s", t.Name, errText)
			}
			continue
		}

		count, err := rdb.Get(ctx, key).Int()
		if err != nil && !errors.Is(err, redis.Nil) {
			log.Printf("health: redis get %s: %v", t.Name, err)
			continue
		}
		if count >= 2 {
			b, _ := json.Marshal(fiber.Map{"name": t.Name})
			_ = rdb.Publish(ctx, "service.up", string(b)).Err()
			sendNotify(notifyURL, "✅ "+t.Name+" recovered", "info")
			log.Printf("✅ %s recovered", t.Name)
		}
		_ = rdb.Del(ctx, key, alertKey).Err()
	}
	return nil
}

// runRetention чистит старые метрики: без этого service_health и
// memory_checks растут неограниченно.
func runRetention() error {
	if db == nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	queries := []string{
		`DELETE FROM service_health WHERE id IN (
			SELECT id FROM service_health
			WHERE checked_at < NOW() - INTERVAL '30 days'
			LIMIT 10000)`,
		`DELETE FROM memory_checks WHERE id IN (
			SELECT id FROM memory_checks
			WHERE checked_at < NOW() - INTERVAL '30 days'
			LIMIT 10000)`,
	}
	for _, q := range queries {
		for {
			res, err := db.Exec(rctx, q)
			if err != nil {
				return err
			}
			if res.RowsAffected() == 0 {
				break
			}
		}
	}
	return nil
}

// runOomCheck спрашивает у notify-service (на хосте) kernel-лог и алертит при OOM.
func runOomCheck() error {
	base := platform.GetEnv("NOTIFY_SERVICE_URL", "http://host.containers.internal:8083")
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", base+"/oom-check", nil)
	if err != nil {
		return err
	}
	applyNotifyToken(req)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var r struct {
		Lines string `json:"lines"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	if strings.TrimSpace(r.Lines) == "" {
		return nil
	}
	log.Printf("🔴 OOM-события:\n%s", r.Lines)
	if rdb != nil {
		_ = rdb.Publish(ctx, "oom.detected", r.Lines).Err()
	}
	sendNotify(base, "🔴 OOM-kill обнаружен:\n"+r.Lines, "error")
	return nil
}

// applyNotifyToken — подписывает запрос к notify-service секретом gateway.
// Без него /send и /oom-check отдают 403 (notify доверяет только loopback
// и gateway с корректным X-Gateway-Token).
func applyNotifyToken(req *http.Request) {
	if t := strings.TrimSpace(platform.GetEnv("NOTIFY_GATEWAY_TOKEN", "")); t != "" {
		req.Header.Set("X-Gateway-Token", t)
	}
}

// adminToken — секрет админ-операций (обновление стека через host-agent).
// Один и тот же ADMIN_TOKEN настроен на gateway, scheduler и host-agent.
func adminToken() string {
	return strings.TrimSpace(platform.GetEnv("ADMIN_TOKEN", ""))
}

// prowlarrBaseURL — адрес только из env, без override из запроса.
func prowlarrBaseURL() (string, error) {
	raw := strings.TrimSpace(platform.GetEnv("PROWLARR_URL", "http://host.containers.internal:9696"))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("invalid PROWLARR_URL")
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

// hostAgentGET — GET к host-agent с сервисным admin-токеном: read-only ручки
// агента тоже закрыты (sensors/smart/disks/cleanup/update).
func hostAgentGET(path string) (*http.Response, error) {
	base := strings.TrimRight(platform.GetEnv("HOST_AGENT_URL", "http://host.containers.internal:9998"), "/")
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	if t := adminToken(); t != "" {
		req.Header.Set("X-Admin-Token", t)
	}
	return (&http.Client{Timeout: 30 * time.Second}).Do(req)
}

// bearerToken — регистронезависимая схема Bearer (RFC 7235).
func bearerToken(h string) string {
	scheme, val, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(val)
}

// adminTokenOK — сверяет X-Admin-Token или Authorization: Bearer с ADMIN_TOKEN
// постоянным временем сравнения. Пустой ADMIN_TOKEN = fail-closed.
func adminTokenOK(c fiber.Ctx) bool {
	expected := adminToken()
	if expected == "" {
		return false
	}
	got := strings.TrimSpace(c.Get("X-Admin-Token"))
	if got == "" {
		got = bearerToken(c.Get("Authorization"))
	}
	if got == "" {
		return false
	}
	// ConstantTimeCompare на разных длинах выходит сразу (оракул длины).
	hg := sha256.Sum256([]byte(got))
	he := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(hg[:], he[:]) == 1
}

// requireAdminToken — POST /update/run больше не открыт для любого
// контейнера в сети: запуск update-stack.sh требует секрет.
func requireAdminToken(c fiber.Ctx) error {
	if adminTokenOK(c) {
		return c.Next()
	}
	return c.Status(403).JSON(fiber.Map{"error": "forbidden: admin token required"})
}

func sendNotify(base, text, severity string) {
	body, _ := json.Marshal(fiber.Map{"text": text, "severity": severity})
	req, err := http.NewRequest("POST", base+"/send", strings.NewReader(string(body)))
	if err != nil {
		log.Printf("⚠️ notify: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	applyNotifyToken(req)
	resp, err := notifyHTTPClient.Do(req)
	if err != nil {
		log.Printf("⚠️ notify: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode == 403 {
		log.Printf("⚠️ notify: 403 — проверь NOTIFY_GATEWAY_TOKEN")
	}
}

// runDiskCheckDetailed проверяет заполненность корня через host-agent и при >85%
// присылает детальный алерт (размеры контейнеров/кэшей).
func runDiskCheckDetailed() error {
	resp, err := hostAgentGET("/disks")
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	var disks []struct {
		Mount   string `json:"mount"`
		Used    string `json:"used"`
		Avail   string `json:"avail"`
		Total   string `json:"total"`
		Percent string `json:"percent"`
	}
	if err := json.Unmarshal(body, &disks); err != nil {
		return err
	}

	pct := 0.0
	var rootUsed, rootAvail, rootTotal string
	for _, d := range disks {
		if d.Mount == "/" {
			pctStr := strings.TrimSuffix(d.Percent, "%")
			pct, _ = strconv.ParseFloat(pctStr, 64)
			rootUsed, rootAvail, rootTotal = d.Used, d.Avail, d.Total
			break
		}
	}
	if pct < 85 {
		return nil
	}

	// Детали: что можно очистить
	details := ""
	if r, err := hostAgentGET("/cleanup/preview"); err == nil {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		r.Body.Close()
		var items []struct {
			Name        string `json:"name"`
			SizeBytes   int64  `json:"size_bytes"`
			Description string `json:"description"`
		}
		if json.Unmarshal(b, &items) == nil {
			for _, it := range items {
				if it.SizeBytes > 0 {
					details += fmt.Sprintf("\n• %s: %.1f GB", it.Description, float64(it.SizeBytes)/1e9)
				}
			}
		}
	}

	text := fmt.Sprintf("⚠️ Диск / заполнен на %.0f%% (%s из %s, свободно %s)%s", pct, rootUsed, rootTotal, rootAvail, details)
	notifyURL := platform.GetEnv("NOTIFY_SERVICE_URL", "http://host.containers.internal:8083")
	sendNotify(notifyURL, text, "warning")
	log.Printf("⚠️ disk-check-detailed: / %.0f%%", pct)
	return nil
}
