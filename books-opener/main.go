package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
)

const (
	listenAddr = ":9999"
	booksDir   = "/mnt/nas/Books"
	youtubeDir = "/mnt/nas/YT_video"
)

var (
	fileOpener   = platform.GetEnv("FILE_OPENER", "foliate")        // для книг
	folderOpener = platform.GetEnv("FOLDER_OPENER", "cosmic-files") // для папок
	videoOpener  = platform.GetEnv("VIDEO_OPENER", "vlc")           // для видео
)

func main() {
	app := fiber.New()

	app.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"GET", "POST", "OPTIONS"},
	}))

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":        "ok",
			"service":       "books-opener",
			"file_opener":   fileOpener,
			"folder_opener": folderOpener,
		})
	})

	// Открыть файл
	app.Get("/open", func(c fiber.Ctx) error {
		name := c.Query("name")
		if name == "" {
			return c.Status(400).JSON(fiber.Map{"error": "name is required"})
		}
		if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
			return c.Status(400).JSON(fiber.Map{"error": "invalid name"})
		}

		filePath := filepath.Join(booksDir, name)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			return c.Status(404).JSON(fiber.Map{"error": "file not found", "path": filePath})
		}

		// Пробуем fileOpener, если не сработает — xdg-open
		if err := run(fileOpener, filePath); err != nil {
			log.Printf("⚠️ %s не сработал: %v, пробую xdg-open", fileOpener, err)
			if err := run("xdg-open", filePath); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
		}

		log.Printf("📂 Открываю: %s", filePath)
		return c.JSON(fiber.Map{"status": "opened", "path": filePath})
	})

	// Открыть папку
	app.Get("/open-folder", func(c fiber.Ctx) error {
		if err := run(folderOpener, booksDir); err != nil {
			log.Printf("⚠️ %s не сработал: %v, пробую xdg-open", folderOpener, err)
			if err := run("xdg-open", booksDir); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
		}

		log.Printf("📂 Открываю папку: %s", booksDir)
		return c.JSON(fiber.Map{"status": "opened", "path": booksDir})
	})

	// Открыть видео в плеере
	app.Get("/open-video", func(c fiber.Ctx) error {
		name := c.Query("name")
		if name == "" {
			return c.Status(400).JSON(fiber.Map{"error": "name is required"})
		}
		if strings.Contains(name, "..") || strings.Contains(name, "/") || strings.Contains(name, "\\") {
			return c.Status(400).JSON(fiber.Map{"error": "invalid name"})
		}
		filePath := filepath.Join(youtubeDir, name)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			return c.Status(404).JSON(fiber.Map{"error": "file not found", "path": filePath})
		}
		if err := run(videoOpener, filePath); err != nil {
			log.Printf("⚠️ %s не сработал: %v, пробую xdg-open", videoOpener, err)
			if err := run("xdg-open", filePath); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
		}
		log.Printf("▶️ Открываю видео: %s", filePath)
		return c.JSON(fiber.Map{"status": "opened", "path": filePath})
	})

	// Открыть медиа по полному пути (фильмы/сериалы)
	app.Get("/open-media", func(c fiber.Ctx) error {
		path := c.Query("path")
		if path == "" {
			return c.Status(400).JSON(fiber.Map{"error": "path is required"})
		}
		allowedPrefixes := []string{
			"/mnt/nas/root/FuzzyHD/Movies/",
			"/mnt/nas/root/FuzzyHD/TV/",
			"/mnt/nas/YT_video/",
		}
		ok := false
		for _, p := range allowedPrefixes {
			if strings.HasPrefix(path, p) {
				ok = true
				break
			}
		}
		if !ok {
			return c.Status(403).JSON(fiber.Map{"error": "path not allowed"})
		}
		if _, err := os.Stat(path); err != nil {
			return c.Status(404).JSON(fiber.Map{"error": "file not found", "path": path})
		}
		if err := run(videoOpener, path); err != nil {
			log.Printf("⚠️ %s не сработал: %v, пробую xdg-open", videoOpener, err)
			if err := run("xdg-open", path); err != nil {
				return c.Status(500).JSON(fiber.Map{"error": err.Error()})
			}
		}
		log.Printf("▶️ Открываю медиа: %s", path)
		return c.JSON(fiber.Map{"status": "opened", "path": path})
	})

	log.Printf("📂 books-opener на %s", listenAddr)
	log.Printf("   Папка с книгами: %s", booksDir)
	log.Printf("   Открываю файлы: %s", fileOpener)
	log.Printf("   Открываю папки: %s", folderOpener)
	log.Fatal(app.Listen(listenAddr))
}

func run(command string, args ...string) error {
	cmd := exec.Command(command, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}
