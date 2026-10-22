package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"golang.org/x/text/encoding/charmap"
)

var (
	rdb *redis.Client
	ctx = context.Background()

	// Все запросы к NAS — с таймаутом: http.Get/PostForm по умолчанию висят вечно.
	nasHTTP = &http.Client{Timeout: 30 * time.Second}
)

func main() {
	if err := platform.ValidateEnv([]string{"NAS_HOST"}); err != nil {
		log.Fatalf("❌ %v", err)
	}
	rdb = redis.NewClient(&redis.Options{
		Addr: platform.GetEnv("REDIS_ADDR", "localhost:6379"),
	})

	app := fiber.New(fiber.Config{
		BodyLimit:    1 << 20,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 70 * time.Second,
		IdleTimeout:  60 * time.Second,
		ErrorHandler: func(c fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			var fe *fiber.Error
			if errors.As(err, &fe) {
				code = fe.Code
			} else {
				log.Printf("http: %v", err)
			}
			msg := http.StatusText(code)
			if msg == "" {
				msg = "error"
			}
			return c.Status(code).JSON(fiber.Map{"error": msg})
		},
	})
	platform.StartPprof()
	// metrics ДО recover: паника должна попасть в метрики как 500.
	app.Use(metricsMiddleware())
	app.Use(recover.New())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("torrent", nil)) })

	// Список доступных папок назначения
	app.Get("/destinations", func(c fiber.Ctx) error {
		raw := platform.GetEnv("TORRENT_DESTINATIONS", "")
		if raw == "" {
			raw = platform.GetEnv("TORRENT_DESTINATION", "homes/FuzzyHD/Music")
		}
		parts := strings.Split(raw, ",")
		dests := []string{}
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				dests = append(dests, p)
			}
		}
		return c.JSON(fiber.Map{"destinations": dests})
	})

	// Определение категории/папки по названию (для UI)
	app.Get("/guess", func(c fiber.Ctx) error {
		title := c.Query("title")
		return c.JSON(fiber.Map{
			"category":    guessCategory(title),
			"destination": guessDestination(title),
		})
	})

	// Профиль RuTracker (рейтинг отдачи)
	app.Get("/rutracker/profile", func(c fiber.Ctx) error {
		refresh := c.Query("refresh") == "1"
		p := getRuTrackerProfile(refresh)
		return c.JSON(p)
	})

	// Добавить торрент
	app.Post("/add", func(c fiber.Ctx) error {
		var body struct {
			Magnet      string `json:"magnet"`
			TorrentURL  string `json:"torrentUrl"`
			Destination string `json:"destination"`
		}
		if err := c.Bind().JSON(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid body"})
		}
		if body.Magnet == "" && body.TorrentURL == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "нужен magnet или torrentUrl"})
		}

		var magnet, torrentURL string
		if body.Magnet != "" {
			m, err := validateMagnet(body.Magnet)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid magnet"})
			}
			magnet = m
		} else {
			u, err := validateTorrentURL(body.TorrentURL)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid torrentUrl"})
			}
			torrentURL = u.String()
		}

		readable := magnetName(magnet)
		if magnet == "" {
			readable = fileNameFromURL(torrentURL)
		}

		dest := ""
		autoDetected := false
		if body.Destination != "" {
			d, err := resolveDestination(body.Destination)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid destination"})
			}
			dest = d
		} else {
			dest = guessDestination(readable)
			autoDetected = true
		}

		if magnet != "" {
			if err := addMagnetToNAS(magnet, dest); err != nil {
				log.Printf("add magnet: %v", err)
				return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "failed to add torrent"})
			}
		} else {
			if err := addTorrentURLToNAS(c.Context(), torrentURL, dest); err != nil {
				log.Printf("add torrent url: %v", err)
				return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "failed to add torrent"})
			}
		}

		if err := rdb.Publish(ctx, "torrent.added", readable).Err(); err != nil {
			log.Printf("⚠️ Redis publish failed: %v", err)
		}

		return c.JSON(fiber.Map{"status": "added", "destination": dest, "auto": autoDetected, "category": guessCategory(readable)})
	})

	// Список торрентов
	app.Get("/list", func(c fiber.Ctx) error {
		tasks, err := listTasks()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"data": fiber.Map{"task": tasks}})
	})

	// Сводная статистика
	app.Get("/stats", func(c fiber.Ctx) error {
		tasks, err := listTasks()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(torrentStats(tasks))
	})

	// Настройки Download Station (лимит одновременных загрузок)
	app.Get("/settings/download-station", func(c fiber.Ctx) error {
		cfg, err := getDSConfig()
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(cfg)
	})

	app.Post("/settings/download-station", func(c fiber.Ctx) error {
		var b struct {
			MaxDownload int `json:"max_download"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.MaxDownload < 0 {
			return c.Status(400).JSON(fiber.Map{"error": "нужен max_download >= 0"})
		}
		if b.MaxDownload > 50 {
			b.MaxDownload = 50
		}
		if err := setDSConfig(b.MaxDownload); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ok", "max_download": b.MaxDownload})
	})

	// Детали торрента
	app.Get("/info/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		tasks, err := listTasks()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		for _, t := range tasks {
			if t["id"] == id {
				return c.JSON(t)
			}
		}
		return c.Status(404).JSON(fiber.Map{"error": "not found"})
	})

	// Трекеры всех торрентов
	app.Get("/trackers", func(c fiber.Ctx) error {
		tasks, err := listTasks()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"trackers": collectTrackers(tasks)})
	})

	// Трекеры конкретного торрента
	app.Get("/trackers/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		tasks, err := listTasks()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		for _, t := range tasks {
			if t["id"] == id {
				if add, ok := t["additional"].(map[string]interface{}); ok {
					return c.JSON(fiber.Map{"trackers": add["tracker"]})
				}
				return c.JSON(fiber.Map{"trackers": []interface{}{}})
			}
		}
		return c.Status(404).JSON(fiber.Map{"error": "not found"})
	})

	// Файлы торрента
	app.Get("/files/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		tasks, err := listTasks()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		for _, t := range tasks {
			if t["id"] == id {
				if add, ok := t["additional"].(map[string]interface{}); ok {
					return c.JSON(fiber.Map{"files": add["file"]})
				}
				return c.JSON(fiber.Map{"files": []interface{}{}})
			}
		}
		return c.Status(404).JSON(fiber.Map{"error": "not found"})
	})

	// Пауза
	app.Post("/pause/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		if err := taskAction("pause", id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "paused", "id": id})
	})

	// Возобновить
	app.Post("/resume/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		if err := taskAction("resume", id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "resumed", "id": id})
	})

	// Удалить
	app.Delete("/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		if err := taskAction("delete", id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "deleted", "id": id})
	})

	log.Println("🚀 torrent-service на :8081")
	log.Fatal(app.Listen(":8081"))
}

// ============================================================
// NAS API
// ============================================================

// resolveDestination превращает ключ назначения в путь на NAS:
//
//	"music" -> TORRENT_DESTINATION_MUSIC, "video" -> TORRENT_DESTINATION_VIDEO,
//	пусто   -> TORRENT_DESTINATION (совместимость), иначе — только путь из
//	allowlist TORRENT_DESTINATIONS/TORRENT_DESTINATION*.
func resolveDestination(dest string) (string, error) {
	dest = strings.TrimSpace(dest)
	switch strings.ToLower(dest) {
	case "":
		return platform.GetEnv("TORRENT_DESTINATION", "homes/FuzzyHD/Music"), nil
	case "music":
		return platform.GetEnv("TORRENT_DESTINATION_MUSIC", "homes/FuzzyHD/Music"), nil
	case "video":
		return platform.GetEnv("TORRENT_DESTINATION_VIDEO", "homes/FuzzyHD/Video"), nil
	}
	if !validDestination(dest) {
		return "", errors.New("invalid destination")
	}
	return dest, nil
}

// validDestination — allowlist из TORRENT_DESTINATIONS/TORRENT_DESTINATION*.
// Запрещает абсолютные пути, "..", управляющие символы и произвольные NAS-пути.
func validDestination(dest string) bool {
	if dest == "" || len(dest) > 512 || strings.HasPrefix(dest, "/") || strings.Contains(dest, "..") {
		return false
	}
	for _, r := range dest {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, p := range allowedDestinations() {
		if p == dest {
			return true
		}
	}
	return false
}

func allowedDestinations() []string {
	raw := []string{
		platform.GetEnv("TORRENT_DESTINATIONS", ""),
		platform.GetEnv("TORRENT_DESTINATION", "homes/FuzzyHD/Music"),
		platform.GetEnv("TORRENT_DESTINATION_MUSIC", "homes/FuzzyHD/Music"),
		platform.GetEnv("TORRENT_DESTINATION_VIDEO", "homes/FuzzyHD/Video"),
		platform.GetEnv("TORRENT_DESTINATION_TV", "homes/FuzzyHD/TV"),
		platform.GetEnv("TORRENT_DESTINATION_MOVIES", "homes/FuzzyHD/Movies"),
		platform.GetEnv("TORRENT_DESTINATION_GAMES", "homes/FuzzyHD/Games"),
		platform.GetEnv("TORRENT_DESTINATION_COURSES", "homes/FuzzyHD/Courses"),
		platform.GetEnv("TORRENT_DESTINATION_BOOKS", "homes/FuzzyHD/Books"),
	}
	var out []string
	seen := make(map[string]struct{})
	for _, group := range raw {
		for _, p := range strings.Split(group, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

var (
	reSeason  = regexp.MustCompile(`(?i)s\d{1,2}\s?e\d{1,2}|season\s?\d|сезон\s?\d`)
	reEpisode = regexp.MustCompile(`(?i)\b\d{1,2}x\d{1,2}\b`)
)

// guessCategory определяет категорию торрента по его названию.
func guessCategory(title string) string {
	t := strings.ToLower(title)

	if reSeason.MatchString(t) || reEpisode.MatchString(t) {
		return "tv"
	}
	if strings.Contains(t, "flac") || strings.Contains(t, "mp3") ||
		strings.Contains(t, "lossless") || strings.Contains(t, "dsd") ||
		strings.Contains(t, "discography") || strings.Contains(t, "дискография") ||
		strings.Contains(t, " OST") || strings.Contains(t, "саундтрек") {
		return "music"
	}
	if strings.Contains(t, "fitgirl") || strings.Contains(t, "repack") ||
		strings.Contains(t, "xatab") || strings.Contains(t, "igruha") ||
		strings.Contains(t, " gog") || strings.Contains(t, "игра") {
		return "games"
	}
	if strings.Contains(t, "udemy") || strings.Contains(t, "course") ||
		strings.Contains(t, "курс") || strings.Contains(t, "лекци") {
		return "courses"
	}
	if strings.Contains(t, "book") || strings.Contains(t, "книг") || strings.Contains(t, ".pdf") ||
		strings.Contains(t, ".epub") || strings.Contains(t, ".fb2") || strings.Contains(t, ".mobi") ||
		strings.Contains(t, " аудиокниг") || strings.Contains(t, "lib.") {
		return "books"
	}
	return "movies"
}

// guessDestination возвращает NAS-путь по названию торрента.
func guessDestination(title string) string {
	switch guessCategory(title) {
	case "tv":
		return platform.GetEnv("TORRENT_DESTINATION_TV", "homes/FuzzyHD/TV")
	case "music":
		return platform.GetEnv("TORRENT_DESTINATION_MUSIC", "homes/FuzzyHD/Music")
	case "games":
		return platform.GetEnv("TORRENT_DESTINATION_GAMES", "homes/FuzzyHD/Games")
	case "courses":
		return platform.GetEnv("TORRENT_DESTINATION_COURSES", "homes/FuzzyHD/Courses")
	case "books":
		return platform.GetEnv("TORRENT_DESTINATION_BOOKS", "homes/FuzzyHD/Books")
	default:
		return platform.GetEnv("TORRENT_DESTINATION_MOVIES", "homes/FuzzyHD/Movies")
	}
}

func getSID() (string, error) {
	host := platform.GetEnv("NAS_HOST", "192.168.0.5")
	user := platform.GetEnv("NAS_USER", "")
	pass := platform.GetEnv("NAS_PASS", "")

	loginURL := fmt.Sprintf(
		"http://%s:5000/webapi/auth.cgi?api=SYNO.API.Auth&version=7&method=login&account=%s&passwd=%s&session=DownloadStation&format=sid",
		host, user, url.QueryEscape(pass),
	)
	resp, err := nasHTTP.Get(loginURL)
	if err != nil {
		return "", fmt.Errorf("login failed: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	sid := extractSID(string(bodyBytes))
	if sid == "" {
		return "", fmt.Errorf("no SID in response: %s", string(bodyBytes))
	}
	return sid, nil
}

// getDSConfig читает конфиг Download Station (в т.ч. http_max_download).
func getDSConfig() (fiber.Map, error) {
	host := platform.GetEnv("NAS_HOST", "192.168.0.5")
	sid, err := getSID()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf(
		"http://%s:5000/webapi/DownloadStation/info.cgi?api=SYNO.DownloadStation.Info&version=1&method=getconfig&_sid=%s",
		host, sid,
	)
	resp, err := nasHTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var res struct {
		Data    map[string]interface{} `json:"data"`
		Success bool                   `json:"success"`
		Error   map[string]interface{} `json:"error"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("parse error: %w (%s)", err, string(body))
	}
	if !res.Success {
		return nil, fmt.Errorf("NAS error: %s", string(body))
	}
	md := 0
	if res.Data != nil {
		if v, ok := res.Data["http_max_download"].(float64); ok {
			md = int(v)
		}
	}
	out := fiber.Map{"max_download": md}
	if res.Data != nil {
		out["raw"] = res.Data
	}
	return out, nil
}

// setDSConfig устанавливает лимит одновременных загрузок Download Station.
func setDSConfig(maxDownload int) error {
	host := platform.GetEnv("NAS_HOST", "192.168.0.5")
	sid, err := getSID()
	if err != nil {
		return err
	}
	form := url.Values{
		"api":               {"SYNO.DownloadStation.Info"},
		"version":           {"1"},
		"method":            {"setserverconfig"},
		"_sid":              {sid},
		"http_max_download": {strconv.Itoa(maxDownload)},
	}
	resp, err := nasHTTP.PostForm(fmt.Sprintf("http://%s:5000/webapi/DownloadStation/info.cgi", host), form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `"success":true`) {
		return nil
	}
	// Некоторые сборки DSM отвечают ошибкой, но всё равно применяют настройку —
	// проверяем фактическое значение.
	if cfg, verr := getDSConfig(); verr == nil {
		if v, ok := cfg["max_download"].(int); ok && v == maxDownload {
			return nil
		}
	}
	return fmt.Errorf("NAS error: %s", string(body))
}

// createAuthorizedAdditionalJSON — значение параметра additional для create:
// задача сразу авторизуется на скачивание (иначе DSM держит её в waiting навсегда)
// и не переходит в seeding после завершения (сохраняем текущее поведение деплоя).
func createAuthorizedAdditionalJSON() string {
	return `{"download_detail":{"create_authorized":true,"seed_after_complete":false}}`
}

// parseCreatedTaskID извлекает id созданной задачи из тела ответа create.
// DSM отвечает по-разному: {"data":{"id":"dbid_123"},"success":true},
// {"data":{"id":["dbid_123"]},"success":true} или просто {"success":true}.
// Возвращает "" когда id в ответе нет.
func parseCreatedTaskID(body string) string {
	var res struct {
		Data struct {
			ID interface{} `json:"id"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		return ""
	}
	switch v := res.Data.ID.(type) {
	case string:
		return v
	case []interface{}:
		if len(v) > 0 {
			if s, ok := v[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// selectCreatedTaskID выбирает из списка задач id той, что была добавлена только что:
// uri в additional.detail совпадает, а create_time не раньше since.
// Возвращает "" если совпадение не уникально (нет ни одной или больше одной).
//
// Известное ограничение точности до секунды: since — это unix-секунды, снятые
// непосредственно ПЕРЕД вызовом create, поэтому ранее существовавшая задача с теми же
// uri И тем же create_time (той же секундой), что и since, пройдёт проверку >= since
// и будет выбрана, если окажется единственным совпадением по id. Это осознанно
// принимаемая, предельно узкая гонка. Держим >= (а не >) намеренно: замена на >
// ошибочно исключила бы задачу, реально созданную в ту же секунду, что и отметка.
func selectCreatedTaskID(tasks []map[string]interface{}, uri string, since int64) string {
	match := ""
	for _, t := range tasks {
		add, _ := t["additional"].(map[string]interface{})
		detail, _ := add["detail"].(map[string]interface{})
		if detail == nil {
			continue
		}
		taskURI, _ := detail["uri"].(string)
		if taskURI != uri {
			continue
		}
		if create_time, _ := detail["create_time"].(float64); int64(create_time) < since {
			continue
		}
		id, _ := t["id"].(string)
		if match != "" && match != id {
			return ""
		}
		match = id
	}
	return match
}

func addMagnetToNAS(magnet, dest string) error {
	host := platform.GetEnv("NAS_HOST", "192.168.0.5")
	sid, err := getSID()
	if err != nil {
		return err
	}

	addURL := fmt.Sprintf("http://%s:5000/webapi/DownloadStation/task.cgi", host)
	data := url.Values{
		"api":         {"SYNO.DownloadStation.Task"},
		"version":     {"1"},
		"method":      {"create"},
		"uri":         {magnet},
		"destination": {dest},
		"_sid":        {sid},
		"additional":  {createAuthorizedAdditionalJSON()},
	}
	// Отметка ДО create — чтобы отличить новую задачу от старой с тем же uri.
	before := time.Now().Unix()
	resp, err := nasHTTP.PostForm(addURL, data)
	if err != nil {
		return fmt.Errorf("add failed: %w", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(bodyBytes), `"success":true`) {
		return fmt.Errorf("NAS error: %s", string(bodyBytes))
	}

	// Задача добавлена; стартуем её best-effort. id определяем ненадёжно:
	// сначала из ответа create, иначе ищем задачу по uri и времени создания.
	taskID := parseCreatedTaskID(string(bodyBytes))
	if taskID == "" {
		tasks, lerr := listTasks()
		if lerr != nil {
			log.Printf("⚠️ task created, but id unknown (list failed: %v) — skip start: %s", lerr, string(bodyBytes))
			return nil
		}
		taskID = selectCreatedTaskID(tasks, magnet, before)
	}
	if taskID == "" {
		// Задача СОЗДАНА, но НЕ ЗАПУЩЕНА — эта функция всё равно возвращает nil.
		// Лучший-усилий контракт старта не позволяет уронить добавление: предупреждающий
		// лог ниже — единственный хук наблюдаемости, и это принятая последствие контракта.
		log.Printf("⚠️ task created, but id not found — skip start: %s", string(bodyBytes))
		return nil
	}
	if err := taskAction("start", taskID); err != nil {
		log.Printf("⚠️ task start %s failed: %v (задача добавлена с create_authorized)", taskID, err)
	}
	return nil
}

// addTorrentURLToNAS добавляет торрент по ссылке:
//   - 1337x отдаёт magnet через 301-редирект — достаём magnet и добавляем через uri
//   - иначе .torrent-файл — переписываем host.containers.internal на LAN-IP хоста
//     и отдаём NAS на скачивание (multipart-upload .torrent на DSM 7.2 возвращает 101)
func addTorrentURLToNAS(rctx context.Context, torrentURL, dest string) error {
	u, err := validateTorrentURL(torrentURL)
	if err != nil {
		return err
	}
	if magnet := resolveMagnet(rctx, u.String()); magnet != "" {
		return addMagnetToNAS(magnet, dest)
	}
	return addMagnetToNAS(rewriteHost(u), dest)
}

// magnetName извлекает читаемое имя (последний dn=) из magnet-ссылки, иначе btih-хеш
func magnetName(magnet string) string {
	if idx := strings.LastIndex(magnet, "dn="); idx != -1 {
		rest := magnet[idx+3:]
		if amp := strings.Index(rest, "&"); amp != -1 {
			rest = rest[:amp]
		}
		if decoded, err := url.QueryUnescape(rest); err == nil {
			return strings.ReplaceAll(decoded, "+", " ")
		}
		return strings.ReplaceAll(rest, "+", " ")
	}
	if idx := strings.Index(magnet, "btih:"); idx != -1 {
		rest := magnet[idx+5:]
		if amp := strings.Index(rest, "&"); amp != -1 {
			rest = rest[:amp]
		}
		return "<magnet " + rest + ">"
	}
	return magnet
}

// fileNameFromURL извлекает имя файла из URL: сначала query-параметр file, иначе путь
func fileNameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if f := u.Query().Get("file"); f != "" {
		return f
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		return rawURL
	}
	return name
}

func listTasks() ([]map[string]interface{}, error) {
	host := platform.GetEnv("NAS_HOST", "192.168.0.5")
	sid, err := getSID()
	if err != nil {
		return nil, err
	}

	listURL := fmt.Sprintf(
		"http://%s:5000/webapi/DownloadStation/task.cgi?api=SYNO.DownloadStation.Task&version=1&method=list&additional=detail,transfer,tracker,file&_sid=%s",
		host, sid,
	)
	resp, err := nasHTTP.Get(listURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Data struct {
			Tasks []map[string]interface{} `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse error: %w (raw: %s)", err, string(body))
	}

	enriched := []map[string]interface{}{}
	for _, task := range result.Data.Tasks {
		size, _ := task["size"].(float64)
		statusStr, _ := task["status"].(string)

		var downloaded, speedDown, speedUp float64
		if additional, ok := task["additional"].(map[string]interface{}); ok {
			if transfer, ok := additional["transfer"].(map[string]interface{}); ok {
				downloaded, _ = transfer["size_downloaded"].(float64)
				speedDown, _ = transfer["speed_download"].(float64)
				speedUp, _ = transfer["speed_upload"].(float64)
			}
		}

		progress := 0.0
		if size > 0 {
			progress = (downloaded / size) * 100
		}
		if statusStr == "finished" || statusStr == "seeding" {
			progress = 100.0
		}

		statusNum := 0
		switch statusStr {
		case "waiting":
			statusNum = 1
		case "downloading":
			statusNum = 2
		case "paused":
			statusNum = 3
		case "finished":
			statusNum = 5
		case "seeding":
			statusNum = 6
		case "error":
			statusNum = 7
		}

		task["progress"] = progress
		task["size_downloaded"] = downloaded
		task["speed_download"] = speedDown
		task["speed_upload"] = speedUp
		task["status_num"] = statusNum

		enriched = append(enriched, task)
	}

	return enriched, nil
}

func torrentStats(tasks []map[string]interface{}) fiber.Map {
	var active, seeding, paused, errorCount, finished int
	var totalSize, downloaded, uploaded float64
	for _, t := range tasks {
		statusStr, _ := t["status"].(string)
		switch statusStr {
		case "waiting", "downloading":
			active++
		case "seeding":
			seeding++
		case "paused":
			paused++
		case "error":
			errorCount++
		case "finished":
			finished++
		}
		if s, ok := t["size"].(float64); ok {
			totalSize += s
		}
		if add, ok := t["additional"].(map[string]interface{}); ok {
			if tr, ok := add["transfer"].(map[string]interface{}); ok {
				if d, ok := tr["size_downloaded"].(float64); ok {
					downloaded += d
				}
				if u, ok := tr["size_uploaded"].(float64); ok {
					uploaded += u
				}
			}
		}
	}
	ratio := 0.0
	if downloaded > 0 {
		ratio = uploaded / downloaded
	}
	return fiber.Map{
		"active":     active,
		"seeding":    seeding,
		"paused":     paused,
		"error":      errorCount,
		"finished":   finished,
		"total_size": totalSize,
		"downloaded": downloaded,
		"uploaded":   uploaded,
		"ratio":      fmt.Sprintf("%.2f", ratio),
	}
}

func collectTrackers(tasks []map[string]interface{}) []interface{} {
	result := []interface{}{}
	for _, t := range tasks {
		if add, ok := t["additional"].(map[string]interface{}); ok {
			if tr, ok := add["tracker"]; ok {
				result = append(result, fiber.Map{"id": t["id"], "title": t["title"], "tracker": tr})
			}
		}
	}
	return result
}

func taskAction(method, id string) error {
	host := platform.GetEnv("NAS_HOST", "192.168.0.5")
	sid, err := getSID()
	if err != nil {
		return err
	}

	actionURL := fmt.Sprintf("http://%s:5000/webapi/DownloadStation/task.cgi", host)
	data := url.Values{
		"api":     {"SYNO.DownloadStation.Task"},
		"version": {"1"},
		"method":  {method},
		"id":      {id},
		"_sid":    {sid},
	}
	resp, err := nasHTTP.PostForm(actionURL, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `"success":true`) {
		return fmt.Errorf("NAS error: %s", string(body))
	}
	return nil
}

func extractSID(jsonStr string) string {
	idx := strings.Index(jsonStr, `"sid":"`)
	if idx == -1 {
		return ""
	}
	start := idx + 7
	end := strings.Index(jsonStr[start:], `"`)
	if end == -1 {
		return ""
	}
	return jsonStr[start : start+end]
}

type RuTrackerProfile struct {
	Username      string  `json:"username"`
	Ratio         float64 `json:"ratio"`
	DownloadedGB  float64 `json:"downloaded_gb"`
	UploadedGB    float64 `json:"uploaded_gb"`
	Bonus         int     `json:"bonus"`
	LastVisit     string  `json:"last_visit"`
	Cached        bool    `json:"cached"`
	StatsDisabled bool    `json:"stats_disabled,omitempty"`
	Error         string  `json:"error,omitempty"`
}

type flareCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

func getRuTrackerProfile(refresh bool) RuTrackerProfile {
	cacheKey := "rutracker:profile"
	if !refresh {
		if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
			var p RuTrackerProfile
			if json.Unmarshal([]byte(cached), &p) == nil {
				p.Cached = true
				return p
			}
		}
	}

	user := platform.GetEnv("RUTRACKER_USER", "")
	pass := platform.GetEnv("RUTRACKER_PASS", "")

	var cookies []flareCookie

	cookieKey := "rutracker:cookies"
	envCookie := platform.GetEnv("RUTRACKER_COOKIE", "")

	if envCookie != "" {
		// 1. Cookie из .env
		cookies = parseCookieHeader(envCookie)
	}
	if len(cookies) == 0 {
		// 2. Кэш сессии в Redis (получена ранее логином)
		if cached, err := rdb.Get(ctx, cookieKey).Result(); err == nil && cached != "" {
			var cc []flareCookie
			if json.Unmarshal([]byte(cached), &cc) == nil && len(cc) > 0 {
				cookies = cc
			}
		}
	}
	if len(cookies) == 0 {
		// 3. Логин через FlareSolverr
		if user == "" || pass == "" {
			return RuTrackerProfile{Error: "RuTracker: нет cookie и не заданы логин/пароль. Запусти 'rt-cookie' в терминале."}
		}
		postData := "login_username=" + url.QueryEscape(user) +
			"&login_password=" + url.QueryEscape(pass) +
			"&login=Login&redirect=index.php"
		_, loginCookies, err := flareRequest("request.post", "https://rutracker.org/forum/login.php", postData, nil)
		if err != nil {
			return RuTrackerProfile{Error: "RuTracker: не удалось войти (" + err.Error() + "). Обнови cookie — запусти 'rt-cookie' в терминале."}
		}
		for _, c := range loginCookies {
			if c.Name == "bb_session" || c.Name == "bb_data" || c.Name == "cf_clearance" {
				cookies = append(cookies, c)
			}
		}
		if len(cookies) == 0 {
			return RuTrackerProfile{Error: "RuTracker: логин не дал сессии. Обнови cookie — запусти 'rt-cookie' в терминале."}
		}
		// Кэшируем сессию, чтобы не логиниться каждый раз
		if data, err := json.Marshal(cookies); err == nil {
			_ = rdb.Set(ctx, cookieKey, string(data), 20*time.Hour).Err()
		}
	}

	// Определить USER_ID
	uid := platform.GetEnv("RUTRACKER_USER_ID", "")
	if uid == "" {
		if indexHTML, _, err := flareRequest("request.get", "https://rutracker.org/forum/index.php", "", cookies); err == nil {
			if m := regexp.MustCompile(`(?i)profile\.php\?[^"']*u=(\d+)`).FindStringSubmatch(indexHTML); m != nil {
				uid = m[1]
			} else if m2 := regexp.MustCompile(`(?i)mode=viewprofile&(?:amp;)?u=(\d+)`).FindStringSubmatch(indexHTML); m2 != nil {
				uid = m2[1]
			}
		}
	}
	if uid == "" {
		return RuTrackerProfile{Error: "RuTracker: не удалось определить USER_ID — укажите RUTRACKER_USER_ID в .env"}
	}
	profileURL := "https://rutracker.org/forum/profile.php?mode=viewprofile&u=" + uid

	html, _, err := flareRequest("request.get", profileURL, "", cookies)
	if err != nil {
		return RuTrackerProfile{Error: "RuTracker: профиль недоступен (cookie устарел). Запусти 'rt-cookie' в терминале."}
	}

	p := parseRuTrackerHTML(html)
	if p.Username == "" && p.Ratio == 0 && !p.StatsDisabled {
		_ = os.WriteFile("/tmp/rutracker-profile.html", []byte(html), 0644)
		if p.Error == "" {
			p.Error = "не удалось распарсить профиль (см. /tmp/rutracker-profile.html)"
		}
	} else {
		if data, err := json.Marshal(p); err == nil {
			_ = rdb.Set(ctx, cacheKey, string(data), time.Hour).Err()
		}
	}
	return p
}

func flareRequest(cmd, targetURL, postData string, cookies []flareCookie) (string, []flareCookie, error) {
	fsURL := platform.GetEnv("FLARESOLVERR_URL", "http://flaresolverr:8191")
	maxTimeout := 120000
	if v, err := strconv.Atoi(platform.GetEnv("FLARESOLVERR_TIMEOUT", "120000")); err == nil && v > 0 {
		maxTimeout = v
	}
	payload := map[string]interface{}{
		"cmd":        cmd,
		"url":        targetURL,
		"maxTimeout": maxTimeout,
	}
	if postData != "" {
		payload["postData"] = postData
	}
	if len(cookies) > 0 {
		payload["cookies"] = cookies
	}
	b, _ := json.Marshal(payload)
	client := &http.Client{Timeout: time.Duration(maxTimeout)*time.Millisecond + 30*time.Second}
	resp, err := client.Post(fsURL+"/v1", "application/json", strings.NewReader(string(b)))
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res struct {
		Solution struct {
			Status   int           `json:"status"`
			Response string        `json:"response"`
			Cookies  []flareCookie `json:"cookies"`
		} `json:"solution"`
	}
	if json.Unmarshal(body, &res) == nil && res.Solution.Response != "" {
		return res.Solution.Response, res.Solution.Cookies, nil
	}
	return "", nil, fmt.Errorf("flaresolverr: %s", string(body))
}

// ruToUTF8 приводит Windows-1251 к UTF-8, если байты не являются валидным UTF-8.
func ruToUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	if out, err := charmap.Windows1251.NewDecoder().String(s); err == nil {
		return out
	}
	return s
}

func parseCookieHeader(s string) []flareCookie {
	var cookies []flareCookie
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if i := strings.Index(part, "="); i > 0 {
			cookies = append(cookies, flareCookie{Name: part[:i], Value: part[i+1:], Domain: ".rutracker.org"})
		}
	}
	return cookies
}

func parseRuTrackerHTML(html string) RuTrackerProfile {
	p := RuTrackerProfile{}
	html = ruToUTF8(html)

	// Username: из <title> (может быть "Nick" или "Nick — ...")
	if m := regexp.MustCompile(`(?is)<title>(.*?)</title>`).FindStringSubmatch(html); m != nil {
		name := strings.TrimSpace(m[1])
		if i := strings.Index(name, "—"); i > 0 {
			name = strings.TrimSpace(name[:i])
		}
		p.Username = name
	}
	// "Профиль пользователя: Nick"
	if p.Username == "" {
		if m := regexp.MustCompile(`(?i)Профиль пользователя:\s*<[^>]*>?\s*([^<\s]+)`).FindStringSubmatch(html); m != nil {
			p.Username = strings.TrimSpace(m[1])
		}
	}
	if m := regexp.MustCompile(`(?i)ratio[^0-9]*([0-9.,]+)`).FindStringSubmatch(html); m != nil {
		p.Ratio, _ = strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
	}
	if m := regexp.MustCompile(`(?i)Скачано[^0-9]*([0-9.,]+)\s*(GB|MB|TB|ГБ|МБ)`).FindStringSubmatch(html); m != nil {
		p.DownloadedGB = parseRuSize(m[1], m[2])
	}
	if m := regexp.MustCompile(`(?i)Отдано[^0-9]*([0-9.,]+)\s*(GB|MB|TB|ГБ|МБ)`).FindStringSubmatch(html); m != nil {
		p.UploadedGB = parseRuSize(m[1], m[2])
	}
	if m := regexp.MustCompile(`(?i)Бонус[^0-9]*([0-9,]+)`).FindStringSubmatch(html); m != nil {
		p.Bonus, _ = strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	}
	if strings.Contains(html, "Статистика отключена") || strings.Contains(html, "статистика отключена") {
		p.StatsDisabled = true
	}
	if p.Username == "" && p.Ratio == 0 && !p.StatsDisabled {
		p.Error = "не удалось распарсить профиль (см. /tmp/rutracker-profile.html)"
	}
	return p
}

func parseRuSize(val, unit string) float64 {
	v, _ := strconv.ParseFloat(strings.ReplaceAll(val, ",", "."), 64)
	switch strings.ToUpper(unit) {
	case "TB", "ТБ":
		return v * 1024
	case "GB", "ГБ":
		return v
	case "MB", "МБ":
		return v / 1024
	default:
		return v
	}
}
