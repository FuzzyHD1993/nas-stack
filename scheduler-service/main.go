package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
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
		Command:  "ssh",
		Args:     []string{"NAS", "sh /volume1/docker/gitea-backups/backup-gitea.sh"},
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
}

var (
	rdb *redis.Client
	db  *pgxpool.Pool
	ctx = context.Background()
)

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

	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("scheduler", nil)) })

	app.Get("/tasks", func(c fiber.Ctx) error {
		result := []fiber.Map{}
		for _, t := range tasks {
			result = append(result, fiber.Map{
				"name":     t.Name,
				"schedule": t.Schedule.String(),
				"command":  t.Command + " " + strings.Join(t.Args, " "),
			})
		}
		return c.JSON(result)
	})

	app.Post("/run/:name", func(c fiber.Ctx) error {
		name := c.Params("name")
		for _, t := range tasks {
			if t.Name == name {
				go execute(t)
				return c.JSON(fiber.Map{"status": "started", "task": name})
			}
		}
		return c.Status(404).JSON(fiber.Map{"error": "task not found"})
	})

	// Ручной запуск бэкапа Gitea. Сама задача защищена от параллельного запуска.
	app.Post("/gitea/backup", func(c fiber.Ctx) error {
		go execute(Task{Name: "gitea-backup", Schedule: 24 * time.Hour})
		return c.JSON(fiber.Map{"status": "started", "task": "gitea-backup"})
	})

	// ---- Health сервисов ----
	app.Get("/health/services", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		rows, err := db.Query(ctx, `
			SELECT DISTINCT ON (service) service, status, COALESCE(response_ms,0), COALESCE(error,''), checked_at
			FROM service_health
			ORDER BY service, checked_at DESC`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		services := []fiber.Map{}
		up, down := 0, 0
		for rows.Next() {
			var name, status, errText string
			var ms int
			var at time.Time
			if rows.Scan(&name, &status, &ms, &errText, &at) == nil {
				if status == "up" {
					up++
				} else {
					down++
				}
				services = append(services, fiber.Map{
					"name": name, "status": status, "response_ms": ms, "error": errText, "last_check": at,
				})
			}
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
		_ = db.QueryRow(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE status='up'),
				COUNT(*) FILTER (WHERE status<>'up')
			FROM (SELECT DISTINCT ON (service) service, status FROM service_health ORDER BY service, checked_at DESC) t`).Scan(&up, &down)
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
		rows, err := db.Query(ctx, `
			SELECT service, status, COALESCE(response_ms,0), COALESCE(error,''), checked_at
			FROM service_health WHERE service=$1 ORDER BY checked_at DESC LIMIT $2`, service, limit)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		for rows.Next() {
			var name, status, errText string
			var ms int
			var at time.Time
			if rows.Scan(&name, &status, &ms, &errText, &at) == nil {
				items = append(items, fiber.Map{"name": name, "status": status, "response_ms": ms, "error": errText, "checked_at": at})
			}
		}
		return c.JSON(fiber.Map{"service": service, "history": items, "count": len(items)})
	})

	// GET /health/memory — последние метрики памяти/горутин по сервисам.
	app.Get("/health/memory", func(c fiber.Ctx) error {
		if db == nil {
			return c.Status(503).JSON(fiber.Map{"error": "БД недоступна"})
		}
		rows, err := db.Query(ctx, `
			SELECT DISTINCT ON (service) service, COALESCE(mem_mb,0), COALESCE(goroutines,0),
			       COALESCE(gc_count,0), checked_at
			FROM memory_checks ORDER BY service, checked_at DESC`)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()
		items := []fiber.Map{}
		alerts := []fiber.Map{}
		for rows.Next() {
			var name string
			var mem, gor, gc int
			var at time.Time
			if rows.Scan(&name, &mem, &gor, &gc, &at) == nil {
				items = append(items, fiber.Map{"service": name, "mem_mb": mem, "goroutines": gor, "gc_count": gc, "checked_at": at})
				// Сравнение с записью ~5 минут назад
				var prevMem, prevGor int
				_ = db.QueryRow(ctx, `
					SELECT COALESCE(mem_mb,0), COALESCE(goroutines,0) FROM memory_checks
					WHERE service=$1 AND checked_at < NOW() - INTERVAL '4 minutes'
					ORDER BY checked_at DESC LIMIT 1`, name).Scan(&prevMem, &prevGor)
				if prevGor > 0 && gor-prevGor >= 20 {
					alerts = append(alerts, fiber.Map{"service": name, "type": "goroutines", "prev": prevGor, "now": gor})
				}
				if prevMem > 0 && mem > prevMem*3/2 && mem-prevMem >= 30 {
					alerts = append(alerts, fiber.Map{"service": name, "type": "memory", "prev_mb": prevMem, "now_mb": mem})
				}
			}
		}
		return c.JSON(fiber.Map{"services": items, "alerts": alerts})
	})

	// ---- Обновление стека (статус из Redis, пишет scripts/update-stack.sh) ----
	app.Get("/update/status", func(c fiber.Ctx) error {
		// Статус обновлений берём у host-agent (на хосте есть git).
		agent := platform.GetEnv("HOST_AGENT_URL", "http://host.containers.internal:9998")
		hc := &http.Client{Timeout: 20 * time.Second}
		if resp, err := hc.Get(agent + "/update/status"); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				var out map[string]any
				if json.NewDecoder(resp.Body).Decode(&out) == nil {
					return c.JSON(out)
				}
			}
		}
		// Fallback: метаданные из Redis.
		last, _ := rdb.Get(ctx, "update:last").Result()
		status, _ := rdb.Get(ctx, "update:status").Result()
		commit, _ := rdb.Get(ctx, "update:commit").Result()
		return c.JSON(fiber.Map{"last": last, "status": status, "commit": commit, "updates_available": 0})
	})
	app.Post("/update/run", requireAdminToken, func(c fiber.Ctx) error {
		agent := platform.GetEnv("HOST_AGENT_URL", "http://host.containers.internal:9998")
		hc := &http.Client{Timeout: 10 * time.Second}
		req, err := http.NewRequest("POST", agent+"/update/run", nil)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		// host-agent свои ручки защищает тем же ADMIN_TOKEN — разметка
		// обязательна, иначе /update/run вернёт 403.
		if t := adminToken(); t != "" {
			req.Header.Set("X-Admin-Token", t)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			log.Printf("⛔ host-agent отказал в /update/run (проверь ADMIN_TOKEN)")
			return c.Status(502).JSON(fiber.Map{"error": "host-agent rejected admin token (check ADMIN_TOKEN on host-agent)"})
		}
		return c.JSON(fiber.Map{"status": "started"})
	})

	// ---- Prowlarr auto-setup (добавляет недостающие публичные индексаторы) ----
	app.Post("/prowlarr/setup", func(c fiber.Ctx) error {
		var cfgReq struct {
			APIKey string `json:"api_key"`
			URL    string `json:"url"`
		}
		_ = c.Bind().JSON(&cfgReq)
		base := strings.TrimRight(platform.GetEnv("PROWLARR_URL", "http://host.containers.internal:9696"), "/")
		if cfgReq.URL != "" {
			base = strings.TrimRight(cfgReq.URL, "/")
		}
		apiKey := platform.GetEnv("PROWLARR_API_KEY", "")
		if cfgReq.APIKey != "" {
			apiKey = cfgReq.APIKey
		}
		if apiKey == "" {
			return c.Status(400).JSON(fiber.Map{"error": "PROWLARR_API_KEY not set"})
		}
		targets := []string{"1337x", "The Pirate Bay", "Byrutor"}
		hc := &http.Client{Timeout: 20 * time.Second}

		getJSON := func(path string, out any) error {
			req, _ := http.NewRequest("GET", base+path, nil)
			req.Header.Set("X-Api-Key", apiKey)
			resp, err := hc.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
			}
			return json.NewDecoder(resp.Body).Decode(out)
		}

		var current []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}
		if err := getJSON("/api/v1/indexer", &current); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "prowlarr: " + err.Error()})
		}
		have := map[string]bool{}
		for _, i := range current {
			have[strings.ToLower(i.Name)] = true
		}

		var schema []map[string]any
		if err := getJSON("/api/v1/indexer/schema", &schema); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "schema: " + err.Error()})
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
			b, _ := json.Marshal(body)
			req, _ := http.NewRequest("POST", base+"/api/v1/indexer", bytes.NewReader(b))
			req.Header.Set("X-Api-Key", apiKey)
			req.Header.Set("Content-Type", "application/json")
			resp, err := hc.Do(req)
			if err != nil {
				errs = append(errs, name+": "+err.Error())
				continue
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				added = append(added, name)
			} else {
				rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				errs = append(errs, fmt.Sprintf("%s: HTTP %d %s", name, resp.StatusCode, strings.TrimSpace(string(rb))))
			}
			resp.Body.Close()
		}
		return c.JSON(fiber.Map{"status": "ok", "added": added, "skipped": skipped, "errors": errs})
	})

	// ---- Бэкапы ----
	app.Get("/backup/list", func(c fiber.Ctx) error {
		return c.JSON(listBackups())
	})
	app.Post("/backup/run", func(c fiber.Ctx) error {
		go func() {
			if err := runPgBackup(); err != nil {
				log.Printf("❌ backup/run: %v", err)
			}
		}()
		return c.JSON(fiber.Map{"status": "started"})
	})
	app.Post("/backup/restore", func(c fiber.Ctx) error {
		var b struct {
			File    string `json:"file"`
			Confirm bool   `json:"confirm"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.File == "" {
			return c.Status(400).JSON(fiber.Map{"error": "нужен file"})
		}
		if err := restoreBackup(b.File, b.Confirm); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": err.Error()})
		}
		log.Printf("♻️ БД восстановлена из %s", b.File)
		return c.JSON(fiber.Map{"status": "restored", "file": b.File})
	})

	log.Println("⏰ scheduler-service на :8084")
	if adminToken() == "" {
		log.Println("⚠️ ADMIN_TOKEN не задан — POST /update/run будет отдавать 403")
	}
	log.Fatal(app.Listen(":8084"))
}

func runTask(t Task) {
	time.Sleep(30 * time.Second)
	for {
		execute(t)
		time.Sleep(t.Schedule)
	}
}

func execute(t Task) {
	log.Printf("▶️  Запускаю: %s", t.Name)
	start := time.Now()

	status := "success"
	if t.Name == "pg-backup" {
		if err := runPgBackup(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ pg-backup: %v", err)
		} else {
			log.Printf("✅ pg-backup завершён")
		}
	} else if t.Name == "wishlist-release-check" {
		if err := runWishlistCheck(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ wishlist-release-check: %v", err)
		} else {
			log.Printf("✅ wishlist-release-check завершён")
		}
	} else if t.Name == "series-check" {
		if err := runSeriesCheck(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ series-check: %v", err)
		} else {
			log.Printf("✅ series-check завершён")
		}
	} else if t.Name == "healthcheck-services" {
		if err := runHealthcheck(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ healthcheck-services: %v", err)
		} else {
			log.Printf("✅ healthcheck-services завершён")
		}
	} else if t.Name == "disk-check-detailed" {
		if err := runDiskCheckDetailed(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ disk-check-detailed: %v", err)
		} else {
			log.Printf("✅ disk-check-detailed завершён")
		}
	} else if t.Name == "memory-check" {
		if err := runMemoryCheck(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ memory-check: %v", err)
		} else {
			log.Printf("✅ memory-check завершён")
		}
	} else if t.Name == "oom-check" {
		if err := runOomCheck(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ oom-check: %v", err)
		} else {
			log.Printf("✅ oom-check завершён")
		}
	} else if t.Name == "gitea-backup" {
		if err := runGiteaBackup(); err != nil {
			status = "error: " + err.Error()
			log.Printf("❌ gitea-backup: %v", err)
		} else {
			log.Printf("✅ gitea-backup завершён")
		}
	} else {
		cmd := exec.Command(t.Command, t.Args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			status = "error: " + err.Error()
		}
		if len(output) > 0 {
			log.Printf("   Вывод: %s", string(output))
		}
	}

	duration := time.Since(start)
	log.Printf("✅ %s завершён за %v: %s", t.Name, duration, status)

	if rdb != nil {
		rdb.Set(context.Background(), "task:"+t.Name+":last", time.Now().Format(time.RFC3339), 7*24*time.Hour)
		rdb.Set(context.Background(), "task:"+t.Name+":status", status, 7*24*time.Hour)
	}
}

var giteaBackupMu sync.Mutex

// giteaBackupCommand builds the SSH command for the NAS backup script.
// Key authentication is preferred; NAS_PASS is an optional fallback and is
// passed through SSHPASS (-e) so it does not appear in the process list.
func giteaBackupCommand(ctx context.Context) (*exec.Cmd, error) {
	host := platform.GetEnv("NAS_HOST", "192.168.0.5")
	user := platform.GetEnv("NAS_USER", "FuzzyHD")
	port := platform.GetEnv("NAS_SSH_PORT", "5022")
	script := platform.GetEnv("NAS_BACKUP_SCRIPT", "/volume1/docker/gitea-backups/backup-gitea.sh")

	sshArgs := []string{
		"-p", port,
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/tmp/gitea_known_hosts",
	}

	keyPath := platform.GetEnv("NAS_SSH_KEY", "/root/.ssh/id_ed25519")
	if info, err := os.Stat(keyPath); err == nil && !info.IsDir() {
		sshArgs = append(sshArgs,
			"-i", keyPath,
			"-o", "IdentitiesOnly=yes",
			"-o", "BatchMode=yes",
		)
		sshArgs = append(sshArgs, user+"@"+host, "sh", script)
		return exec.CommandContext(ctx, "ssh", sshArgs...), nil
	}

	password := os.Getenv("NAS_PASS")
	if password == "" {
		return nil, fmt.Errorf("SSH key %q не найден, а NAS_PASS пуст", keyPath)
	}
	sshArgs = append(sshArgs, user+"@"+host, "sh", script)
	cmd := exec.CommandContext(ctx, "sshpass", append([]string{"-e", "ssh"}, sshArgs...)...)
	cmd.Env = append(os.Environ(), "SSHPASS="+password)
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
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gitea backup failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if out := strings.TrimSpace(string(output)); out != "" {
		log.Printf("   Gitea backup: %s", out)
	}
	return nil
}

// runPgBackup делает дамп PostgreSQL в daily/weekly/monthly с ротацией.
func runPgBackup() error {
	pgURL := platform.GetEnv("POSTGRES_URL", "postgres://nas:nas_secret@postgres:5432/nas_history?sslmode=disable")
	baseDir := platform.GetEnv("BACKUP_DIR", "/backups/postgres")

	dailyDir := filepath.Join(baseDir, "daily")
	weeklyDir := filepath.Join(baseDir, "weekly")
	monthlyDir := filepath.Join(baseDir, "monthly")
	_ = os.MkdirAll(dailyDir, 0o755)
	_ = os.MkdirAll(weeklyDir, 0o755)
	_ = os.MkdirAll(monthlyDir, 0o755)

	now := time.Now()

	dailyFile := filepath.Join(dailyDir, "nas_history-"+now.Format("20060102-150405")+".sql.gz")
	if err := dumpPG(pgURL, dailyFile); err != nil {
		return err
	}
	rotateDir(dailyDir, 7)

	if now.Weekday() == time.Sunday {
		week := now.Format("2006-W02")
		if err := dumpPG(pgURL, filepath.Join(weeklyDir, "nas_history-"+week+".sql.gz")); err != nil {
			log.Printf("⚠️ weekly backup failed: %v", err)
		}
		rotateDir(weeklyDir, 4)
	}
	if now.Day() == 1 {
		month := now.Format("2006-01")
		if err := dumpPG(pgURL, filepath.Join(monthlyDir, "nas_history-"+month+".sql.gz")); err != nil {
			log.Printf("⚠️ monthly backup failed: %v", err)
		}
		rotateDir(monthlyDir, 12)
	}

	if rdb != nil {
		_ = rdb.Publish(context.Background(), "backup.completed", filepath.Base(dailyFile)).Err()
	}
	return nil
}

func dumpPG(pgURL, out string) error {
	shellCmd := "pg_dump --no-owner --no-privileges '" + pgURL + "' | gzip > '" + out + "'"
	cmd := exec.Command("sh", "-c", shellCmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(out)
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(output)))
	}
	if fi, statErr := os.Stat(out); statErr != nil || fi.Size() == 0 {
		return errNoBackup
	}
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
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql.gz") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			arr = append(arr, fiber.Map{
				"file":       filepath.Join(dir, e.Name()),
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

// restoreBackup восстанавливает БД из gzip-дампа.
func restoreBackup(file string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("нужно confirm=true")
	}
	baseDir := platform.GetEnv("BACKUP_DIR", "/backups/postgres")
	clean := filepath.Clean(file)
	if !strings.HasPrefix(clean, filepath.Clean(baseDir)+string(os.PathSeparator)) {
		return fmt.Errorf("путь вне каталога бэкапов")
	}
	if _, err := os.Stat(clean); err != nil {
		return fmt.Errorf("файл не найден")
	}
	pgURL := platform.GetEnv("POSTGRES_URL", "postgres://nas:nas_secret@postgres:5432/nas_history?sslmode=disable")
	shellCmd := "gunzip -c '" + clean + "' | psql '" + pgURL + "'"
	cmd := exec.Command("sh", "-c", shellCmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restore: %v: %s", err, strings.TrimSpace(string(output)))
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
	body, _ := io.ReadAll(resp.Body)
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
	body, _ := io.ReadAll(resp.Body)
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
		b2, _ := io.ReadAll(r2.Body)
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
			_ = resp.Body.Close()
		} else {
			errText = err.Error()
		}

		if db != nil {
			_, _ = db.Exec(ctx, `
				INSERT INTO service_health (service, status, response_ms, error)
				VALUES ($1,$2,$3,$4)`, t.Name, status, responseMs, errText)
		}

		key := "health:fails:" + t.Name
		if status == "down" {
			count, _ := rdb.Incr(ctx, key).Result()
			_ = rdb.Expire(ctx, key, 5*time.Minute).Err()
			if count >= 2 {
				b, _ := json.Marshal(fiber.Map{"name": t.Name, "error": errText})
				_ = rdb.Publish(ctx, "service.down", string(b)).Err()
				sendNotify(notifyURL, "❌ "+t.Name+" down: "+errText, "error")
				log.Printf("❌ %s down: %s", t.Name, errText)
			}
		} else {
			count, _ := rdb.Get(ctx, key).Int()
			if count >= 2 {
				b, _ := json.Marshal(fiber.Map{"name": t.Name})
				_ = rdb.Publish(ctx, "service.up", string(b)).Err()
				sendNotify(notifyURL, "✅ "+t.Name+" recovered", "info")
				log.Printf("✅ %s recovered", t.Name)
			}
			_ = rdb.Del(ctx, key).Err()
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

// adminTokenOK — сверяет X-Admin-Token или Authorization: Bearer с ADMIN_TOKEN
// постоянным временем сравнения. Пустой ADMIN_TOKEN = fail-closed.
func adminTokenOK(c fiber.Ctx) bool {
	expected := adminToken()
	if expected == "" {
		return false
	}
	got := strings.TrimSpace(c.Get("X-Admin-Token"))
	if got == "" {
		if auth := c.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("⚠️ notify: %v", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode == 403 {
		log.Printf("⚠️ notify: 403 — проверь NOTIFY_GATEWAY_TOKEN")
	}
}

// runDiskCheckDetailed проверяет заполненность корня через host-agent и при >85%
// присылает детальный алерт (размеры контейнеров/кэшей).
func runDiskCheckDetailed() error {
	base := platform.GetEnv("HOST_AGENT_URL", "http://host.containers.internal:9998")
	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Get(base + "/disks")
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
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
	if r, err := client.Get(base + "/cleanup/preview"); err == nil {
		b, _ := io.ReadAll(r.Body)
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
