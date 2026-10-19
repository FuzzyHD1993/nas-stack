package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/client"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/logger"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// notifyToken — секрет gateway→notify-service (X-Gateway-Token).
// package-level, т.к. proxyHandler вызывается вне main().
var notifyToken string

// adminToken — ADMIN_TOKEN: доступ к host-exec ручкам (/notify/update/*,
// /notify/env*, /scheduler/update/run, host-agent /update/*).
// package-level по той же причине, что и notifyToken.
var adminToken string

// loadAdminToken читает ADMIN_TOKEN из окружения. Токен НЕ генерируется на лету:
// неизвестный токен, напечатанный в лог, означает, что UI работать не будет, а
// админские ручки молча закроются. Лучше упасть на старте с понятным текстом,
// чем работать «в половинку». Сгенерировать: openssl rand -hex 32 -> ADMIN_TOKEN.
func loadAdminToken() string {
	t := strings.TrimSpace(platform.GetEnv("ADMIN_TOKEN", ""))
	if t == "" {
		log.Fatal("❌ ADMIN_TOKEN не задан. Без него закрыты host-exec ручки " +
			"(/notify/update/*, /notify/env*, /scheduler/update/*). " +
			"Добавьте в .env: ADMIN_TOKEN=<openssl rand -hex 32>")
	}
	return t
}

// bearerOrAdminHeader — достаёт токен из Authorization: Bearer <t> или X-Admin-Token.
func bearerOrAdminHeader(c fiber.Ctx) string {
	if v := strings.TrimSpace(c.Get("X-Admin-Token")); v != "" {
		return v
	}
	if auth := strings.TrimSpace(c.Get("Authorization")); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// adminTokenOK — сверяет присланный токен с ADMIN_TOKEN (constant time).
// Пустой ADMIN_TOKEN = fail-closed (никто не проходит).
func adminTokenOK(c fiber.Ctx) bool {
	expected := adminToken
	if expected == "" {
		return false
	}
	got := bearerOrAdminHeader(c)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// adminAuth — единственный вход на ручки с побочными эффектами на хосте
// (обновление стека, откат, редактор .env, reload). Раньше любой, кто
// дотянулся до gateway, получал от proxy подставленный X-Gateway-Token —
// то есть P0 host-exec без единого пароля.
func adminAuth(c fiber.Ctx) error {
	if adminTokenOK(c) {
		return c.Next()
	}
	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
		"error": "admin token required: send 'Authorization: Bearer <ADMIN_TOKEN>'",
	})
}

// adminProtectedPath — ручки проксируемых сервисов, дающие host-exec или
// доступ к секретам. Вызывается и в proxyHandler (defense in depth: даже если
// префикс middleware не совпал, запрос с host-exec не уйдёт дальше).
//
//   - notify:    /update/* (запуск update-stack.sh, git reset), /env* (чтение и
//     запись .env хоста — там NAS_PASS, JWT_SECRET и т.д.)
//   - scheduler: /update/* (кроме read-only /update/status — git-метаданные)
func adminProtectedPath(service, method, path string) bool {
	if !adminProxyService(service) {
		return false
	}
	p := strings.ToLower(strings.Trim(strings.TrimSpace(path), "/"))
	if p == "" {
		return false
	}
	isEnv := p == "env" || strings.HasPrefix(p, "env/")
	isUpdate := p == "update" || strings.HasPrefix(p, "update/")
	if !isEnv && !isUpdate {
		return false
	}
	// /update/status только читает git-метаданные и вызывается баннером
	// обновлений в UI — оставляем открытым, host-exec там нет.
	if service == "scheduler" && p == "update/status" && (method == "GET" || method == "HEAD") {
		return false
	}
	return true
}

// adminProxyServices — сервисы, у которых есть закрываемые ручки.
func adminProxyService(name string) bool { return name == "notify" || name == "scheduler" }

// serviceTokenPath — ручки, куда gateway подставляет сервисный X-Gateway-Token
// БЕЗ adminAuth: /notify/send — обычные уведомления (их шлют сервисы стека:
// stats/media/torrent), host-exec там нет, и закрывать их админ-токеном нельзя.
func serviceTokenPath(service, method, path string) bool {
	p := strings.ToLower(strings.Trim(strings.TrimSpace(path), "/"))
	if service != "notify" {
		return false
	}
	return p == "send" && (method == "POST" || method == "PUT")
}

func main() {
	app := fiber.New(fiber.Config{
		AppName: "NAS Gateway",
	})
	platform.StartPprof()
	app.Use(metricsMiddleware())
	app.Use(logger.New())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	statsURL := platform.GetEnv("STATS_SERVICE_URL", "http://localhost:8082")
	torrentURL := platform.GetEnv("TORRENT_SERVICE_URL", "http://localhost:8081")
	notifyURL := platform.GetEnv("NOTIFY_SERVICE_URL", "http://localhost:8083")
	schedulerURL := platform.GetEnv("SCHEDULER_SERVICE_URL", "http://localhost:8084")
	historyURL := platform.GetEnv("HISTORY_SERVICE_URL", "http://localhost:8085")
	authURL := platform.GetEnv("AUTH_SERVICE_URL", "http://localhost:8086")
	booksURL := platform.GetEnv("BOOKS_SERVICE_URL", "http://localhost:8087")
	searchURL := platform.GetEnv("SEARCH_SERVICE_URL", "http://localhost:8088")
	youtubeURL := platform.GetEnv("YOUTUBE_SERVICE_URL", "http://localhost:8089")
	mediaURL := platform.GetEnv("MEDIA_SERVICE_URL", "http://localhost:8091")
	musicURL := platform.GetEnv("MUSIC_SERVICE_URL", "http://localhost:8092")
	jwtSecret := []byte(platform.GetEnv("JWT_SECRET", "change-me-in-production"))
	authEnabled := platform.GetEnv("AUTH_ENABLED", "false") == "true"

	// Секрет для доступа к notify-service host-exec ручкам (/update/*, /env).
	notifyToken = strings.TrimSpace(platform.GetEnv("NOTIFY_GATEWAY_TOKEN", ""))
	if notifyToken == "" {
		log.Println("⚠️ NOTIFY_GATEWAY_TOKEN не задан — /notify/update/* и /notify/env вернут 403")
	}
	adminToken = loadAdminToken()

	// ============================================================
	// ПУБЛИЧНЫЕ РОУТЫ
	// ============================================================
	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("gateway", nil)) })

	app.Post("/auth/login", func(c fiber.Ctx) error {
		cc := client.New()
		cfg := client.Config{
			Timeout: 5 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    c.Body(),
		}
		resp, err := cc.Post(authURL+"/login", cfg)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "auth-service недоступен"})
		}
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.Get("/auth/verify", func(c fiber.Ctx) error {
		header := map[string]string{}
		if auth := c.Get("Authorization"); auth != "" {
			header["Authorization"] = auth
		}
		cc := client.New()
		resp, err := cc.Get(authURL+"/verify", client.Config{
			Timeout: 5 * time.Second,
			Header:  header,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "auth-service недоступен"})
		}
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	// ============================================================
	// JWT MIDDLEWARE (опциональный)
	// ============================================================
	requireAuth := func(c fiber.Ctx) error {
		if !authEnabled {
			return c.Next()
		}
		// Валидный ADMIN_TOKEN — самостоятельный доступ: он идёт в том же
		// заголовке Authorization, что и JWT, и в админ-вызовах UI заменяет его.
		if adminTokenOK(c) {
			return c.Next()
		}

		auth := c.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			return c.Status(401).JSON(fiber.Map{"error": "no token"})
		}
		tokenStr := strings.TrimPrefix(auth, "Bearer ")

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			return c.Status(401).JSON(fiber.Map{"error": "invalid token"})
		}
		return c.Next()
	}

	// ============================================================
	// ПРОКСИОБЛОЖКИ (публичный, без auth)
	// ============================================================
	app.Get("/books/cover", func(c fiber.Ctx) error {
		coverURL := c.Query("url")
		if coverURL == "" {
			return c.Status(400).SendString("url is required")
		}
		cc := client.New()
		resp, err := cc.Get(booksURL+"/cover?url="+url.QueryEscape(coverURL), client.Config{
			Timeout: 15 * time.Second,
		})
		if err != nil {
			return c.Status(502).SendString(err.Error())
		}
		contentType := resp.Header("Content-Type")
		if contentType == "" {
			contentType = "image/jpeg"
		}
		c.Set("Content-Type", contentType)
		c.Set("Cache-Control", "public, max-age=86400")
		return c.Send(resp.Body())
	})

	// ============================================================
	// ЗАЩИЩЁННЫЕ РОУТЫ
	// ============================================================
	app.Use("/status", requireAuth)
	app.Use("/stats/*", requireAuth)
	app.Use("/torrent/*", requireAuth)
	app.Use("/notify/*", requireAuth)
	app.Use("/scheduler/*", requireAuth)
	app.Use("/history/*", requireAuth)
	app.Use("/books/*", requireAuth)
	app.Use("/search/*", requireAuth)
	app.Use("/youtube/*", requireAuth)
	app.Use("/media/*", requireAuth)
	app.Use("/music/*", requireAuth)

	// ADMIN_TOKEN на всё, что даёт host-exec или доступ к .env хоста.
	// /scheduler/update/* НЕ закрываем целиком: /scheduler/update/status —
	// read-only (git-метаданные), его дёргает баннер обновлений в UI без
	// админ-токена. Хост-исполняющие ручки перечисляем точечно; полный
	// перечень дополнительно проверяется в proxyHandler (adminProtectedPath).
	for _, p := range []string{
		"/notify/update", "/notify/update/*",
		"/notify/env", "/notify/env/*",
		"/scheduler/update/run", "/scheduler/update/rollback",
	} {
		app.Use(p, adminAuth)
	}

	app.Get("/status", func(c fiber.Ctx) error {
		services := map[string]string{
			"stats":     statsURL,
			"torrent":   torrentURL,
			"notify":    notifyURL,
			"scheduler": schedulerURL,
			"history":   historyURL,
			"auth":      authURL,
			"books":     booksURL,
			"search":    searchURL,
			"youtube":   youtubeURL,
			"media":     mediaURL,
			"music":     musicURL,
		}
		result := fiber.Map{}
		for name, url := range services {
			cc := client.New()
			resp, err := cc.Get(url+"/health", client.Config{
				Timeout: 2 * time.Second,
			})
			if err != nil {
				result[name] = "down"
			} else {
				result[name] = string(resp.Body())
			}
		}
		return c.JSON(result)
	})

	// Умный поиск: парсим фразу → параметры → media-service /movies/smart.
	// Регистрируется до catch-all /search/*.
	app.Post("/search/smart", func(c fiber.Ctx) error {
		var req struct {
			Query string `json:"query"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "bad json"})
		}
		params := parseSmartQuery(req.Query)
		if len(params) == 0 {
			return c.JSON(fiber.Map{"movies": []fiber.Map{}, "count": 0, "params": params})
		}
		body, _ := json.Marshal(params)
		cc := client.New()
		resp, err := cc.Post(mediaURL+"/movies/smart", client.Config{
			Timeout: 15 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    body,
		})
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		return c.Status(resp.StatusCode()).Send(resp.Body())
	})

	app.All("/stats/*", proxyHandler(statsURL, "stats"))
	app.All("/torrent/*", proxyHandler(torrentURL, "torrent"))
	app.All("/notify/*", proxyHandler(notifyURL, "notify"))
	app.All("/scheduler/*", proxyHandler(schedulerURL, "scheduler"))
	app.All("/history/*", proxyHandler(historyURL, "history"))
	app.All("/books/*", proxyHandler(booksURL, "books"))
	app.All("/search/*", proxyHandler(searchURL, "search"))
	app.All("/youtube/*", proxyHandler(youtubeURL, "youtube"))
	app.All("/media/*", proxyHandler(mediaURL, "media"))
	app.All("/music/*", proxyHandler(musicURL, "music"))

	// Глобальный поиск (параллельно по всем сервисам)
	app.Get("/search-all", func(c fiber.Ctx) error {
		q := c.Query("q")
		if q == "" {
			return c.Status(400).JSON(fiber.Map{"error": "q is required"})
		}
		query := url.QueryEscape(q)

		result := fiber.Map{}
		var mu sync.Mutex
		var wg sync.WaitGroup

		fetch := func(key, target, field string) {
			defer wg.Done()
			cc := client.New()
			resp, err := cc.Get(target, client.Config{Timeout: 30 * time.Second})
			if err != nil {
				return
			}
			var parsed map[string]interface{}
			if json.Unmarshal(resp.Body(), &parsed) == nil {
				mu.Lock()
				result[key] = parsed[field]
				mu.Unlock()
			}
		}

		wg.Add(4)
		go fetch("movies", mediaURL+"/movies?q="+query+"&limit=20", "items")
		go fetch("tv", mediaURL+"/tv?q="+query+"&limit=20", "items")
		go fetch("torrents", searchURL+"/search?q="+query, "results")
		go fetch("books", booksURL+"/search?q="+query, "books")
		wg.Wait()

		return c.JSON(result)
	})

	log.Println("🚀 gateway на :8080")
	log.Fatal(app.Listen(":8080"))
}

// ============================================================
// УНИВЕРСАЛЬНЫЙ ПРОКСИ
// ============================================================
func proxyHandler(baseURL, name string) fiber.Handler {
	return func(c fiber.Ctx) error {
		path := c.Params("*")
		target := baseURL + "/" + path

		// Host-exec через прокси — только по ADMIN_TOKEN (дублирует middleware,
		// чтобы защита не зависела от семантики префиксов Use).
		if adminProxyService(name) && adminProtectedPath(name, c.Method(), path) && !adminTokenOK(c) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "admin token required: send 'Authorization: Bearer <ADMIN_TOKEN>'",
			})
		}

		// Сохраняем query-параметры
		if qs := string(c.Request().URI().QueryString()); qs != "" {
			target += "?" + qs
		}

		cc := client.New()
		timeout := 30 * time.Second
		if name == "stats" {
			timeout = 60 * time.Second // /stats/nas: du -sh по CIFS может быть >30s
		}
		cfg := client.Config{
			Timeout: timeout,
			Header:  map[string]string{},
		}

		c.Request().Header.VisitAll(func(k, v []byte) {
			key := string(k)
			if key != "Host" && key != "Authorization" {
				cfg.Header[key] = string(v)
			}
		})

		// Внутренние служебные заголовки ставятся ПОСЛЕ копирования клиентских —
		// иначе клиент мог бы подсунуть свой X-Gateway-Token/X-Admin-Token.
		// Слепая подстановка убрана: сервисный секрет уходит ТОЛЬКО на
		// admin-ручки, куда клиент уже прошёл adminAuth. Для обычных /notify/*
		// (health, oom-check) секрет больше не подставляется.
		// Для scheduler отправляем X-Admin-Token, т.к. Authorization у прокси
		// обрезается (JWT не должен утекать), а scheduler обязан сам
		// перепроверить токен перед вызовом host-agent.
		delete(cfg.Header, "X-Gateway-Token")
		delete(cfg.Header, "X-Admin-Token")
		if adminProxyService(name) && adminProtectedPath(name, c.Method(), path) {
			// admin-ручка: сервисный + админский токен (клиент уже прошёл adminAuth).
			if name == "notify" && notifyToken != "" {
				cfg.Header["X-Gateway-Token"] = notifyToken
			}
			if adminToken != "" {
				cfg.Header["X-Admin-Token"] = adminToken
			}
		} else if serviceTokenPath(name, c.Method(), path) {
			// /notify/send: только сервисный токен, adminAuth не требуется.
			if notifyToken != "" {
				cfg.Header["X-Gateway-Token"] = notifyToken
			}
		}

		body := c.Body()
		if len(body) > 0 {
			cfg.Body = body
		}

		var resp *client.Response
		var err error

		switch c.Method() {
		case "GET":
			resp, err = cc.Get(target, cfg)
		case "POST":
			resp, err = cc.Post(target, cfg)
		case "PUT":
			resp, err = cc.Put(target, cfg)
		case "DELETE":
			resp, err = cc.Delete(target, cfg)
		case "PATCH":
			resp, err = cc.Patch(target, cfg)
		default:
			return c.Status(405).SendString("Method Not Allowed")
		}

		if err != nil {
			return c.Status(502).JSON(fiber.Map{
				"error":   "service unavailable",
				"service": name,
				"detail":  err.Error(),
			})
		}

		if ct := resp.Header("Content-Type"); ct != "" {
			c.Set("Content-Type", ct)
		}
		if loc := resp.Header("Location"); loc != "" {
			c.Set("Location", loc)
		}
		if cc := resp.Header("Cache-Control"); cc != "" {
			c.Set("Cache-Control", cc)
		}
		return c.Status(resp.StatusCode()).Send(resp.Body())
	}
}

// parseSmartQuery превращает свободную фразу в параметры поиска фильмов.
func parseSmartQuery(q string) map[string]interface{} {
	q = strings.ToLower(q)
	p := map[string]interface{}{}
	genreMap := map[string]string{
		"комеди": "комеди", "comedy": "комеди", "боевик": "боевик", "action": "боевик",
		"драма": "драм", "drama": "драм", "фантаст": "фантаст", "sci-fi": "фантаст",
		"триллер": "триллер", "thriller": "триллер", "ужас": "ужас", "horror": "ужас",
		"мелодрам": "мелодрам", "romance": "мелодрам", "анимац": "анимац", "мульт": "анимац",
		"детектив": "детектив", "mystery": "детектив", "криминал": "криминал", "crime": "криминал",
	}
	for k, v := range genreMap {
		if strings.Contains(q, k) {
			p["genre"] = v
			break
		}
	}
	if strings.Contains(q, "коротк") || strings.Contains(q, "быстр") {
		p["runtime_max"] = 90
	}
	if strings.Contains(q, "длинн") {
		p["runtime_min"] = 150
	}
	if m := regexp.MustCompile(`(\d+)\s*мин`).FindStringSubmatch(q); len(m) == 2 {
		if n, _ := strconv.Atoi(m[1]); n > 0 {
			p["runtime_max"] = n + 10
		}
	}
	if strings.Contains(q, "шедевр") {
		p["rating_min"] = 8.0
	} else if strings.Contains(q, "хорош") || strings.Contains(q, "лучш") {
		p["rating_min"] = 7.0
	}
	if strings.Contains(q, "лёгк") || strings.Contains(q, "легк") || strings.Contains(q, "весел") {
		if _, ok := p["genre"]; !ok {
			p["genre"] = "комеди"
		}
		if _, ok := p["rating_min"]; !ok {
			p["rating_min"] = 6.0
		}
	}
	if strings.Contains(q, "серьёзн") || strings.Contains(q, "серьезн") || strings.Contains(q, "тяжёл") || strings.Contains(q, "тяжел") {
		if _, ok := p["genre"]; !ok {
			p["genre"] = "драм"
		}
	}
	if y := regexp.MustCompile(`(19|20)\d{2}`).FindString(q); y != "" {
		if n, _ := strconv.Atoi(y); n > 0 {
			p["year_from"] = n
			p["year_to"] = n
		}
	}
	p["limit"] = 50
	return p
}
