package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

var (
	rdb *redis.Client
	ctx = context.Background()

	mu        sync.Mutex
	processes = map[string]*exec.Cmd{}
)

func main() {
	rdb = redis.NewClient(&redis.Options{
		Addr: platform.GetEnv("REDIS_ADDR", "localhost:6379"),
	})

	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("youtube", nil)) })

	// SponsorBlock: сегменты для пропуска (sponsor/selfpromo/...).
	app.Get("/sponsorblock", func(c fiber.Ctx) error {
		videoID := c.Query("video_id")
		if videoID == "" {
			return c.Status(400).JSON(fiber.Map{"error": "video_id required"})
		}
		apiURL := "https://sponsor.ajay.app/api/skipSegments?videoID=" + url.QueryEscape(videoID) +
			"&categories=" + url.QueryEscape(`["sponsor","selfpromo","interaction"]`)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(apiURL)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error(), "segments": []interface{}{}})
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		var segments []map[string]interface{}
		_ = json.Unmarshal(body, &segments)
		return c.JSON(fiber.Map{"segments": segments, "video_id": videoID})
	})

	// Скачать видео
	app.Post("/download", func(c fiber.Ctx) error {
		var body struct {
			URL     string `json:"url"`
			Format  string `json:"format"`
			Quality string `json:"quality"`
		}
		if err := c.Bind().JSON(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		if body.URL == "" {
			return c.Status(400).JSON(fiber.Map{"error": "нужен url"})
		}
		jobID, err := startDownload(body.URL, body.Format, body.Quality)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"job_id": jobID})
	})

	// Список задач
	app.Get("/jobs", func(c fiber.Ctx) error {
		jobs, err := listJobs()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"jobs": jobs})
	})

	// Одна задача
	app.Get("/jobs/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		job, err := getJob(id)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(job)
	})

	// Отменить
	app.Delete("/jobs/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		if err := cancelJob(id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "cancelled", "id": id})
	})

	// Открыть в VLC
	app.Post("/open/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		if err := openJob(id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "opened"})
	})

	log.Println("🚀 youtube-service на :8089")
	log.Fatal(app.Listen(":8089"))
}

// ============================================================
// ЗАДАЧИ
// ============================================================

func startDownload(url, format, quality string) (string, error) {
	jobID := uuid.NewString()
	now := time.Now().Format(time.RFC3339)

	job := map[string]interface{}{
		"url":         url,
		"format":      format,
		"quality":     quality,
		"status":      "starting",
		"progress":    "0",
		"title":       "",
		"path":        "",
		"error":       "",
		"created_at":  now,
		"started_at":  now,
		"finished_at": "",
	}

	key := "youtube:job:" + jobID
	pipe := rdb.Pipeline()
	pipe.HSet(ctx, key, job)
	pipe.SAdd(ctx, "youtube:jobs", jobID)
	if _, err := pipe.Exec(ctx); err != nil {
		return "", err
	}

	go runDownload(jobID, url, format, quality)

	return jobID, nil
}

func runDownload(jobID, url, format, quality string) {
	key := "youtube:job:" + jobID
	dir := platform.GetEnv("YOUTUBE_DIR", "/youtube")
	potURL := platform.GetEnv("POT_PROVIDER_URL", "http://127.0.0.1:4416")

	rdb.HSet(ctx, key, "status", "downloading")

	args := []string{
		"--extractor-args", "youtube:player_client=mweb",
		"--extractor-args", "youtubepot-bgutilhttp:base_url=" + potURL,
		"--js-runtimes", "deno",
		"--remote-components", "ejs:github",
		"--sleep-requests", "3",
		"--retries", "10",
		"--fragment-retries", "10",
		"--socket-timeout", "60",
		"-P", dir,
		"-o", "%(title)s [%(id)s].%(ext)s",
	}

	if format == "mp3" {
		args = append(args, "-x", "--audio-format", "mp3")
	} else {
		args = append(args, "--sponsorblock-remove", "sponsor,selfpromo,interaction")
		if quality == "" || quality == "best" {
			args = append(args, "-f", "bestvideo+bestaudio/best", "--merge-output-format", "mp4")
		} else {
			args = append(args, "-f", "bestvideo[height<="+quality+"]+bestaudio/best", "--merge-output-format", "mp4")
		}
	}

	args = append(args, url)

	cmd := exec.Command("yt-dlp", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		rdb.HSet(ctx, key, "status", "error", "error", err.Error())
		rdb.HSet(ctx, key, "finished_at", time.Now().Format(time.RFC3339))
		return
	}
	cmd.Stderr = cmd.Stdout

	registerProcess(jobID, cmd)
	defer unregisterProcess(jobID)

	if err := cmd.Start(); err != nil {
		rdb.HSet(ctx, key, "status", "error", "error", err.Error())
		rdb.HSet(ctx, key, "finished_at", time.Now().Format(time.RFC3339))
		return
	}

	progressRe := regexp.MustCompile(`\[download\]\s+([0-9.]+)%`)
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if m := progressRe.FindStringSubmatch(line); m != nil {
			rdb.HSet(ctx, key, "progress", m[1])
		}
		if strings.HasPrefix(line, "[download] Destination: ") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "[download] Destination: "))
			rdb.HSet(ctx, key, "title", baseNameNoExt(p))
		}
	}

	err = cmd.Wait()
	if err != nil {
		status, _ := rdb.HGet(ctx, key, "status").Result()
		if status != "cancelled" {
			rdb.HSet(ctx, key, "status", "error", "error", err.Error())
		}
		rdb.HSet(ctx, key, "finished_at", time.Now().Format(time.RFC3339))
		return
	}

	finalPath := newestFile(dir)
	rdb.HSet(ctx, key, "status", "done", "progress", "100", "path", finalPath)
	if title, _ := rdb.HGet(ctx, key, "title").Result(); title == "" {
		rdb.HSet(ctx, key, "title", baseNameNoExt(finalPath))
	}
	rdb.HSet(ctx, key, "finished_at", time.Now().Format(time.RFC3339))
}

func cancelJob(id string) error {
	key := "youtube:job:" + id
	if rdb.Exists(ctx, key).Val() == 0 {
		return fmt.Errorf("задача не найдена")
	}
	mu.Lock()
	cmd := processes[id]
	mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	rdb.HSet(ctx, key, "status", "cancelled", "finished_at", time.Now().Format(time.RFC3339))
	return nil
}

func openJob(id string) error {
	key := "youtube:job:" + id
	p, err := rdb.HGet(ctx, key, "path").Result()
	if err != nil || p == "" {
		return fmt.Errorf("путь не найден")
	}
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("файл не найден: %s", p)
	}
	go func() {
		if err := exec.Command("xdg-open", p).Start(); err != nil {
			_ = exec.Command("vlc", p).Start()
		}
	}()
	return nil
}

func listJobs() ([]map[string]string, error) {
	ids, err := rdb.SMembers(ctx, "youtube:jobs").Result()
	if err != nil {
		return nil, err
	}
	jobs := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		fields, err := rdb.HGetAll(ctx, "youtube:job:"+id).Result()
		if err != nil {
			continue
		}
		fields["id"] = id
		jobs = append(jobs, fields)
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i]["created_at"] > jobs[j]["created_at"]
	})
	return jobs, nil
}

func getJob(id string) (map[string]string, error) {
	fields, err := rdb.HGetAll(ctx, "youtube:job:"+id).Result()
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("задача не найдена")
	}
	fields["id"] = id
	return fields, nil
}

// ============================================================
// ХЕЛПЕРЫ
// ============================================================

func registerProcess(id string, cmd *exec.Cmd) {
	mu.Lock()
	processes[id] = cmd
	mu.Unlock()
}

func unregisterProcess(id string) {
	mu.Lock()
	delete(processes, id)
	mu.Unlock()
}

func newestFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = filepath.Join(dir, e.Name())
		}
	}
	return newest
}

func baseNameNoExt(p string) string {
	b := filepath.Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}
