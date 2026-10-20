package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
)

const (
	openerGrace = 1500 * time.Millisecond
	maxNameLen  = 255
)

var (
	listenAddr = platform.GetEnv("OPENER_BIND", "127.0.0.1:9999")
	booksDir   = platform.GetEnv("BOOKS_DIR", "/mnt/nas/Books")
	youtubeDir = platform.GetEnv("YT_DIR", "/mnt/nas/YT_video")

	fileOpener   = platform.GetEnv("FILE_OPENER", "foliate")
	folderOpener = platform.GetEnv("FOLDER_OPENER", "cosmic-files")
	videoOpener  = platform.GetEnv("VIDEO_OPENER", "vlc")

	openerToken = strings.TrimSpace(platform.GetEnv("OPENER_TOKEN", ""))
	allowAnon   = strings.EqualFold(strings.TrimSpace(platform.GetEnv("OPENER_ALLOW_ANONYMOUS", "false")), "true")

	mediaRoots = []string{
		platform.GetEnv("MEDIA_MOVIES_HOST_DIR", "/mnt/nas/root/FuzzyHD/Movies"),
		platform.GetEnv("MEDIA_TV_HOST_DIR", "/mnt/nas/root/FuzzyHD/TV"),
		youtubeDir,
	}
)

var (
	errBadPath  = errors.New("bad path")
	errNotFound = errors.New("not found")

	bookExts = map[string]bool{
		".epub": true, ".fb2": true, ".pdf": true, ".mobi": true,
		".djvu": true, ".cbz": true, ".azw3": true, ".txt": true,
	}
	videoExts = map[string]bool{
		".mkv": true, ".mp4": true, ".avi": true, ".mov": true, ".webm": true,
		".m4v": true, ".ts": true, ".mpg": true, ".mpeg": true, ".wmv": true,
	}

	openSlots = make(chan struct{}, 2)
	openMu    sync.Mutex
	lastOpen  = map[string]time.Time{}
)

func main() {
	if listenAddr == "" {
		log.Fatal("❌ OPENER_BIND пуст")
	}
	// Fail-closed: не-loopback bind без токена — это host-exec для всей сети.
	if !isLoopbackAddr(listenAddr) && openerToken == "" && !allowAnon {
		log.Fatalf("❌ OPENER_TOKEN не задан при bind %s — отказ старта. "+
			"Задайте OPENER_TOKEN (или OPENER_ALLOW_ANONYMOUS=true только если порт опубликован на loopback)", listenAddr)
	}

	app := fiber.New(fiber.Config{
		AppName:      "books-opener",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  30 * time.Second,
		BodyLimit:    4 * 1024,
	})
	app.Use(recover.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins: openerOrigins(),
		AllowMethods: []string{fiber.MethodPost, fiber.MethodOptions},
		AllowHeaders: []string{"Content-Type", "X-Opener", "X-Opener-Token"},
		MaxAge:       600,
	}))
	app.Use(limiter.New(limiter.Config{
		Max:        30,
		Expiration: time.Minute,
		KeyGenerator: func(c fiber.Ctx) string {
			return c.IP()
		},
		LimitReached: func(c fiber.Ctx) error {
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "too many requests"})
		},
	}))

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"status": "ok", "service": "books-opener"})
	})

	// Только POST: GET с побочным эффектом — это drive-by/CSRF.
	app.Post("/open", requireOpener, handleOpenBook)
	app.Post("/open-folder", requireOpener, handleOpenFolder)
	app.Post("/open-video", requireOpener, handleOpenVideo)
	app.Post("/open-media", requireOpener, handleOpenMedia)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	listenErr := make(chan error, 1)
	go func() {
		log.Printf("📂 books-opener на %s", listenAddr)
		if err := app.Listen(listenAddr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
			listenErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-listenErr:
		log.Printf("listen: %v", err)
	}
	if err := app.ShutdownWithTimeout(5 * time.Second); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// openerOrigins — allowlist источников дашборда (не "*").
func openerOrigins() []string {
	raw := strings.TrimSpace(platform.GetEnv("OPENER_ORIGINS", ""))
	if raw == "" {
		raw = strings.TrimSpace(platform.GetEnv("OPENER_ORIGIN", ""))
	}
	if raw == "" {
		return []string{"http://localhost:8090", "http://127.0.0.1:8090"}
	}
	out := []string{}
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

func originAllowed(origin string) bool {
	for _, o := range openerOrigins() {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireOpener: кастомный заголовок заставляет браузер сделать preflight
// (чужой origin отсекается CORS), токен — для сервис-сервис вызовов.
func requireOpener(c fiber.Ctx) error {
	if c.Get("X-Opener") != "1" {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
	}
	if origin := strings.TrimSpace(c.Get("Origin")); origin != "" && !originAllowed(origin) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "forbidden"})
	}
	if openerToken != "" &&
		subtle.ConstantTimeCompare([]byte(c.Get("X-Opener-Token")), []byte(openerToken)) != 1 {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	return c.Next()
}

func handleOpenBook(c fiber.Ctx) error {
	target, err := resolveUnder(booksDir, c.Query("name"), bookExts)
	if err != nil {
		return pathError(c, err)
	}
	return openTarget(c, target, fileOpener)
}

func handleOpenVideo(c fiber.Ctx) error {
	target, err := resolveUnder(youtubeDir, c.Query("name"), videoExts)
	if err != nil {
		return pathError(c, err)
	}
	return openTarget(c, target, videoOpener)
}

func handleOpenFolder(c fiber.Ctx) error {
	target, err := containDir(booksDir)
	if err != nil {
		return pathError(c, err)
	}
	return openTarget(c, target, folderOpener)
}

func handleOpenMedia(c fiber.Ctx) error {
	target, err := resolveMedia(c.Query("path"))
	if err != nil {
		return pathError(c, err)
	}
	return openTarget(c, target, videoOpener)
}

// resolveUnder — имя файла внутри фиксированного корня.
func resolveUnder(root, name string, exts map[string]bool) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxNameLen ||
		name != filepath.Base(name) || name == "." || name == ".." ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") || hasControl(name) {
		return "", errBadPath
	}
	if !exts[strings.ToLower(filepath.Ext(name))] {
		return "", errBadPath
	}
	real, err := containReal(root, filepath.Join(root, name))
	if err != nil {
		return "", err
	}
	if !exts[strings.ToLower(filepath.Ext(real))] {
		return "", errBadPath
	}
	return real, nil
}

// resolveMedia — абсолютный путь внутри одного из разрешённых корней.
func resolveMedia(raw string) (string, error) {
	if raw == "" || !filepath.IsAbs(raw) || hasControl(raw) {
		return "", errBadPath
	}
	clean := filepath.Clean(raw)
	root := ""
	for _, r := range mediaRoots {
		r = filepath.Clean(strings.TrimSpace(r))
		if r == "" || r == "." || r == string(os.PathSeparator) {
			continue
		}
		if clean == r {
			return "", errBadPath // сам корень — не файл
		}
		if strings.HasPrefix(clean, r+string(os.PathSeparator)) {
			root = r
			break
		}
	}
	if root == "" || !videoExts[strings.ToLower(filepath.Ext(clean))] {
		return "", errBadPath
	}
	real, err := containReal(root, clean)
	if err != nil {
		return "", err
	}
	if !videoExts[strings.ToLower(filepath.Ext(real))] {
		return "", errBadPath
	}
	return real, nil
}

// containReal снимает симлинки и проверяет, что результат остался внутри root.
func containReal(root, candidate string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errNotFound
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errNotFound
		}
		return "", errBadPath
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errBadPath
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", errNotFound
	}
	if !info.Mode().IsRegular() {
		return "", errBadPath
	}
	return real, nil
}

func containDir(root string) (string, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errNotFound
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", errNotFound
	}
	if !info.IsDir() {
		return "", errBadPath
	}
	return real, nil
}

func pathError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, errNotFound):
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	case errors.Is(err, errBadPath):
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "path not allowed"})
	default:
		log.Printf("opener: resolve: %v", err)
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
	}
}

func openTarget(c fiber.Ctx, target, primary string) error {
	if recentlyOpened(target) {
		return c.JSON(fiber.Map{"status": "already_opened"})
	}
	select {
	case openSlots <- struct{}{}:
		defer func() { <-openSlots }()
	default:
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{"error": "too many opens"})
	}

	if err := openWithFallback(target, primary, "xdg-open"); err != nil {
		log.Printf("opener: %q: %v", target, err) // путь и ошибка — только в лог
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "open failed"})
	}
	return c.JSON(fiber.Map{"status": "opened"})
}

// openWithFallback: успех = процесс прожил openerGrace; мгновенный выход
// с ошибкой -> следующий opener. Wait() обязателен, иначе зомби.
func openWithFallback(target string, openers ...string) error {
	var lastErr error
	for _, opener := range openers {
		if opener == "" {
			continue
		}
		cmd := exec.Command(opener, target)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			lastErr = fmt.Errorf("%s: %w", opener, err)
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			if err == nil {
				return nil
			}
			lastErr = fmt.Errorf("%s: %w", opener, err)
		case <-time.After(openerGrace):
			go func() { <-done }()
			return nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no opener configured")
	}
	return lastErr
}

func recentlyOpened(target string) bool {
	now := time.Now()
	openMu.Lock()
	defer openMu.Unlock()
	if t, ok := lastOpen[target]; ok && now.Sub(t) < 3*time.Second {
		return true
	}
	lastOpen[target] = now
	if len(lastOpen) > 512 {
		for k, v := range lastOpen {
			if now.Sub(v) > time.Minute {
				delete(lastOpen, k)
			}
		}
	}
	return false
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || r == 0
	})
}
