package main

import (
	"embed"
	"html"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/client"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/logger"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

//go:embed static
var staticFS embed.FS

// passThroughAuth — копирует входящий Authorization в исходящий запрос.
// Нужно для /api/notify/* и /api/scheduler/*: gateway проверяет по этому же
// заголовку ADMIN_TOKEN (host-exec ручки /notify/update/*, /notify/env*,
// /scheduler/update/*), иначе UI получил бы 401 и не смог бы ни обновить
// стек, ни отредактировать .env. Секреты в другие прокси не копируются.
func passThroughAuth(c fiber.Ctx, header map[string]string) map[string]string {
	if header == nil {
		header = map[string]string{}
	}
	if auth := strings.TrimSpace(c.Get("Authorization")); auth != "" {
		header["Authorization"] = auth
	}
	return header
}

// proxyClient — единый HTTP-клиент для всех прокси-запросов.
// client.New() на каждый запрос создавал отдельный пул соединений и горутины,
// которые не закрывались → утечка. Один клиент = один пул.
var proxyClient = client.New()

func main() {
	app := fiber.New()
	platform.StartPprof()
	app.Use(logger.New())
	setupRoutes(app)

	staticRoot, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static embed: %v", err)
	}
	app.Get("/static/*", adaptor.HTTPHandler(
		http.StripPrefix("/static", http.FileServer(http.FS(staticRoot)))))

	// Защита: если .env оказался каталогом (bind-mount без исходного файла) —
	// пробуем заменить пустым файлом, чтобы wizard мог сохранить конфиг.
	if info, err := os.Stat(envPath()); err == nil && info.IsDir() {
		if entries, _ := os.ReadDir(envPath()); len(entries) == 0 {
			if err := os.Remove(envPath()); err != nil {
				log.Printf("⚠️ .env — каталог, удалить не удалось: %v (создайте файл .env на хосте)", err)
			} else if err := os.WriteFile(envPath(), []byte{}, 0o644); err != nil {
				log.Printf("⚠️ .env — не удалось создать файл: %v", err)
			}
		}
	}

	gatewayURL := platform.GetEnv("GATEWAY_URL", "http://localhost:8080")

	app.Get("/", func(c fiber.Ctx) error {
		// Первичная настройка: принудительно (?setup=1) или автоматически
		// (SETUP_AUTO_REDIRECT=true и .env отсутствует/невалиден).
		if c.Query("setup") == "1" {
			return c.Redirect().Status(fiber.StatusFound).To("/setup")
		}
		if platform.GetEnv("SETUP_AUTO_REDIRECT", "false") == "true" && needsSetup() {
			return c.Redirect().Status(fiber.StatusFound).To("/setup")
		}
		data, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).SendString("index.html: " + err.Error())
		}
		// Публичные URL/пути подставляем из env (для браузера).
		page := string(data)
		page = strings.ReplaceAll(page, "http://localhost:9999",
			html.EscapeString(platform.GetEnv("BOOKS_OPENER_PUBLIC_URL", "http://localhost:9999")))
		page = strings.ReplaceAll(page, "/mnt/nas/YT_video",
			html.EscapeString(platform.GetEnv("YOUTUBE_HOST_DIR", "/mnt/nas/YT_video")))
		authEnabled := "false"
		if platform.GetEnv("AUTH_ENABLED", "false") == "true" {
			authEnabled = "true"
		}
		page = strings.ReplaceAll(page, "__NAS_AUTH_ENABLED__", authEnabled)
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.SendString(page)
	})

	app.Get("/manifest.json", func(c fiber.Ctx) error {
		data, err := staticFS.ReadFile("static/manifest.json")
		if err != nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(data)
	})

	app.Get("/sw.js", func(c fiber.Ctx) error {
		data, err := staticFS.ReadFile("static/sw.js")
		if err != nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}
		c.Set("Content-Type", "application/javascript")
		return c.Send(data)
	})

	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("web-dashboard", nil)) })

	// ============================================================
	// ОБЩИЕ ПРОКСИ
	// ============================================================
	app.Get("/api/status", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/status", client.Config{Timeout: 5 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "gateway недоступен"})
		}
		defer resp.Close()
		return c.Send(resp.Body())
	})

	// ============================================================
	// АВТОРИЗАЦИЯ
	// ============================================================
	app.Post("/api/auth/login", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/auth/login", client.Config{
			Timeout: 5 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "auth-service недоступен"})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/auth/verify", func(c fiber.Ctx) error {
		header := map[string]string{}
		if auth := c.Get("Authorization"); auth != "" {
			header["Authorization"] = auth
		}
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/auth/verify", client.Config{
			Timeout: 5 * time.Second,
			Header:  header,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/auth/profiles", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/auth/profiles", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})
	app.Post("/api/auth/profiles", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/auth/profiles", client.Config{
			Timeout: 10 * time.Second, Header: map[string]string{"Content-Type": "application/json"}, Body: c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})
	app.Post("/api/auth/profiles/:id/select", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/auth/profiles/"+c.Params("id")+"/select", client.Config{
			Timeout: 10 * time.Second, Header: map[string]string{"Content-Type": "application/json"}, Body: c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})
	app.Get("/api/stats", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/stats/all", client.Config{Timeout: 5 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "stats недоступен"})
		}
		defer resp.Close()
		return c.Send(resp.Body())
	})

	app.Get("/api/stats/*", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/stats/"+c.Params("*"), client.Config{Timeout: 30 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "stats недоступен"})
		}
		defer resp.Close()
		return c.Send(resp.Body())
	})

	app.Post("/api/stats/*", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/stats/" + c.Params("*")
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Post(target, client.Config{
			Timeout: 120 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "stats недоступен"})
		}
		defer resp.Close()
		return c.Send(resp.Body())
	})

	app.Get("/api/destinations", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/torrent/destinations", client.Config{
			Timeout: 5 * time.Second,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Send(resp.Body())
	})

	// ============================================================
	// ТОРРЕНТЫ
	// ============================================================
	app.Get("/api/torrents", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/torrent/list", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/torrent/rutracker/profile", func(c fiber.Ctx) error {
		u := gatewayURL + "/torrent/rutracker/profile"
		if c.Query("refresh") == "1" {
			u += "?refresh=1"
		}
		cc := proxyClient
		resp, err := cc.Get(u, client.Config{Timeout: 40 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/torrent/stats", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/torrent/stats", client.Config{Timeout: 15 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/torrents", func(c fiber.Ctx) error {
		cc := proxyClient
		cfg := client.Config{
			Timeout: 30 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		}
		resp, err := cc.Post(gatewayURL+"/torrent/add", cfg)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/torrents/:id/:action", func(c fiber.Ctx) error {
		id := c.Params("id")
		action := c.Params("action")
		cc := proxyClient

		var resp *client.Response
		var err error

		switch action {
		case "pause":
			resp, err = cc.Post(gatewayURL+"/torrent/pause/"+id, client.Config{Timeout: 10 * time.Second})
		case "resume":
			resp, err = cc.Post(gatewayURL+"/torrent/resume/"+id, client.Config{Timeout: 10 * time.Second})
		case "delete":
			resp, err = cc.Delete(gatewayURL+"/torrent/"+id, client.Config{Timeout: 10 * time.Second})
		default:
			return c.Status(400).JSON(fiber.Map{"error": "unknown action"})
		}

		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// ============================================================
	// ПОИСК ТОРРЕНТОВ (Prowlarr)
	// ============================================================
	app.Get("/api/search", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/search/search"
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 60 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/search/smart", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/search/smart", client.Config{
			Timeout: 30 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/indexers", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/search/indexers", client.Config{Timeout: 30 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// ============================================================
	// КНИГИ
	// ============================================================
	app.Get("/api/books/search", func(c fiber.Ctx) error {
		query := c.Query("q")
		if query == "" {
			return c.Status(400).JSON(fiber.Map{"error": "query is required"})
		}
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/books/search?q="+url.QueryEscape(query), client.Config{
			Timeout: 60 * time.Second,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/books/download", func(c fiber.Ctx) error {
		cc := proxyClient
		cfg := client.Config{
			Timeout: 60 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		}
		resp, err := cc.Post(gatewayURL+"/books/download", cfg)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/books/downloaded", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/books/downloaded", client.Config{
			Timeout: 10 * time.Second,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/books/history", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/history/events?limit=50", client.Config{
			Timeout: 10 * time.Second,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/books/cover", func(c fiber.Ctx) error {
		coverURL := c.Query("url")
		if coverURL == "" {
			return c.Status(400).SendString("url is required")
		}
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/books/cover?url="+url.QueryEscape(coverURL), client.Config{
			Timeout: 20 * time.Second,
		})
		if err != nil {
			return c.Status(502).SendString(err.Error())
		}
		defer resp.Close()
		contentType := resp.Header("Content-Type")
		if contentType == "" {
			contentType = "image/jpeg"
		}
		c.Set("Content-Type", contentType)
		c.Set("Cache-Control", "public, max-age=86400")
		return c.Send(resp.Body())
	})

	// Отдача файла книги: числовой id → прокси FB2 с локального зеркала,
	// иначе — файл из библиотеки для EPUB-читалки.
	app.Get("/api/books/read/:filename", func(c fiber.Ctx) error {
		fn := c.Params("filename")
		numeric := fn != ""
		for _, r := range fn {
			if r < '0' || r > '9' {
				numeric = false
				break
			}
		}
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/books/read/"+fn, client.Config{Timeout: 120 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		ct := resp.Header("Content-Type")
		if ct == "" {
			if numeric {
				ct = "application/fb2+xml; charset=utf-8"
			} else {
				ct = "application/epub+zip"
			}
		}
		c.Set("Content-Type", ct)
		c.Set("Cache-Control", "no-cache")
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// ============================================================
	// YOUTUBE
	// ============================================================
	app.Get("/api/youtube/jobs", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/youtube/jobs", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/youtube/download", func(c fiber.Ctx) error {
		cc := proxyClient
		cfg := client.Config{
			Timeout: 30 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		}
		resp, err := cc.Post(gatewayURL+"/youtube/download", cfg)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Delete("/api/youtube/jobs/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		cc := proxyClient
		resp, err := cc.Delete(gatewayURL+"/youtube/jobs/"+id, client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/youtube/open/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/youtube/open/"+id, client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/youtube/sponsorblock", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/youtube/sponsorblock"
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 15 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error(), "segments": []interface{}{}})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// ============================================================
	// МЕДИА
	// ============================================================
	app.Get("/api/media/movies", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/media/movies"
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/movies/:id", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/movies/"+c.Params("id"), client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/tv", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/media/tv"
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/genres", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/genres", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/years", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/years", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/library/stats", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/library/stats", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/tv/:id", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/tv/"+c.Params("id"), client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/poster", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/media/poster"
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 20 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		ct := resp.Header("Content-Type")
		if ct == "" {
			ct = "image/jpeg"
		}
		c.Set("Content-Type", ct)
		c.Set("Cache-Control", "public, max-age=86400")
		return c.Send(resp.Body())
	})

	app.Post("/api/media/scan", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/media/scan", client.Config{Timeout: 120 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/media/open/:id", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/media/open/"+c.Params("id"), client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/stats", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/stats", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/scan/status", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/scan/status", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/tv/:id/seasons", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/tv/"+c.Params("id")+"/seasons", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/tv/:id/season/:season", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/tv/"+c.Params("id")+"/season/"+c.Params("season"), client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/episode/:id", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/episode/"+c.Params("id"), client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/episode/:id/stream", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/episode/"+c.Params("id")+"/stream", client.Config{Timeout: 120 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		c.Set("Content-Type", resp.Header("Content-Type"))
		return c.Send(resp.Body())
	})

	app.Get("/api/media/episode/:id/stream/transcoded", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/episode/"+c.Params("id")+"/stream/transcoded", client.Config{Timeout: 120 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		c.Set("Content-Type", resp.Header("Content-Type"))
		return c.Send(resp.Body())
	})

	app.Get("/api/media/episode/:id/subtitles", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/episode/"+c.Params("id")+"/subtitles", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		c.Set("Content-Type", resp.Header("Content-Type"))
		return c.Send(resp.Body())
	})

	app.Get("/api/media/episode/:id/thumbnail", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/episode/"+c.Params("id")+"/thumbnail", client.Config{Timeout: 30 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		c.Set("Content-Type", resp.Header("Content-Type"))
		return c.Send(resp.Body())
	})

	app.Post("/api/media/episode/:id/open", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/media/episode/"+c.Params("id")+"/open", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/episode/:id/next", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/episode/"+c.Params("id")+"/next", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/subtitles/list/:id", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/subtitles/list/"+c.Params("id"), client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/subtitles/:id/:idx", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/subtitles/"+c.Params("id")+"/"+c.Params("idx"), client.Config{Timeout: 30 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		c.Set("Content-Type", resp.Header("Content-Type"))
		return c.Send(resp.Body())
	})

	app.Post("/api/media/progress", func(c fiber.Ctx) error {
		cc := proxyClient
		cfg := client.Config{
			Timeout: 10 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		}
		resp, err := cc.Post(gatewayURL+"/media/progress", cfg)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/progress/continue", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/progress/continue", client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/progress/:type/:id", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/media/progress/"+c.Params("type")+"/"+c.Params("id"), client.Config{Timeout: 10 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/media/match/search", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/media/match/search"
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 20 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/media/match/:id", func(c fiber.Ctx) error {
		cc := proxyClient
		cfg := client.Config{
			Timeout: 30 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		}
		resp, err := cc.Post(gatewayURL+"/media/match/"+c.Params("id"), cfg)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/media/match/:id/reset", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/media/match/"+c.Params("id")+"/reset", client.Config{Timeout: 20 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/search-all", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/search-all"
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 60 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// Прокси истории/статистики
	app.Get("/api/history/*", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/history/" + c.Params("*")
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 20 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// Прокси планировщика (health, backups)
	app.Get("/api/scheduler/*", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/scheduler/" + c.Params("*")
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{
			Timeout: 20 * time.Second,
			Header:  passThroughAuth(c, nil),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})
	app.Post("/api/scheduler/*", func(c fiber.Ctx) error {
		cc := proxyClient
		header := passThroughAuth(c, map[string]string{"Content-Type": "application/json"})
		resp, err := cc.Post(gatewayURL+"/scheduler/"+c.Params("*"), client.Config{
			Timeout: 120 * time.Second,
			Header:  header,
			Body:    c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/notify/*", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/notify/" + c.Params("*")
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{
			Timeout: 120 * time.Second,
			Header:  passThroughAuth(c, nil),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})
	app.Post("/api/notify/*", func(c fiber.Ctx) error {
		cc := proxyClient
		header := passThroughAuth(c, map[string]string{"Content-Type": "application/json"})
		resp, err := cc.Post(gatewayURL+"/notify/"+c.Params("*"), client.Config{
			Timeout: 120 * time.Second,
			Header:  header,
			Body:    c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// Универсальный GET-прокси медиа: people, smart-shelves, similar, collections,
	// photos (groups/thumbnail/full). Регистрируется после явных маршрутов.
	app.Get("/api/media/*", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/media/" + c.Params("*")
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 60 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		if ct := resp.Header("Content-Type"); ct != "" {
			c.Set("Content-Type", ct)
		}
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// POST-прокси медиа: rename/preview, rename/apply и прочее.
	app.Post("/api/media/*", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/media/"+c.Params("*"), client.Config{
			Timeout: 120 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// DELETE-прокси медиа: wishlist/:id и прочее.
	app.Delete("/api/media/*", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Delete(gatewayURL+"/media/"+c.Params("*"), client.Config{Timeout: 30 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// Универсальный GET-прокси торрентов: info/:id, trackers/:id, files/:id.
	app.Get("/api/torrent/*", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Get(gatewayURL+"/torrent/"+c.Params("*"), client.Config{Timeout: 30 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/torrent/*", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/torrent/"+c.Params("*"), client.Config{
			Timeout: 30 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/api/music/*", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := gatewayURL + "/music/" + c.Params("*")
		if qs != "" {
			target += "?" + qs
		}
		cc := proxyClient
		resp, err := cc.Get(target, client.Config{Timeout: 30 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		if ct := resp.Header("Content-Type"); ct != "" {
			c.Set("Content-Type", ct)
		}
		if cc := resp.Header("Cache-Control"); cc != "" {
			c.Set("Cache-Control", cc)
		}
		if loc := resp.Header("Location"); loc != "" {
			c.Set("Location", loc)
		}
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Post("/api/music/*", func(c fiber.Ctx) error {
		cfg := client.Config{
			Timeout: 15 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		}
		cc := proxyClient
		resp, err := cc.Post(gatewayURL+"/music/"+c.Params("*"), cfg)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Delete("/api/music/*", func(c fiber.Ctx) error {
		cc := proxyClient
		resp, err := cc.Delete(gatewayURL+"/music/"+c.Params("*"), client.Config{Timeout: 15 * time.Second})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Close()
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	log.Println("🌐 web-dashboard на :8090")
	log.Fatal(app.Listen(":8090"))
}
