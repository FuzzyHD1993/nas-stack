package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/textproto"
	"net/url"
	pathpkg "path"
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
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/golang-jwt/jwt/v5"
)

// notifyToken — секрет gateway→notify-service (X-Gateway-Token).
// historyToken — секрет gateway→history-service (X-Gateway-Token).
// adminToken — ADMIN_TOKEN: доступ к host-exec ручкам (/notify/update/*,
// /notify/env*, /scheduler/update/run, host-agent /update/*).
// Читаются из горутин HTTP-обработчиков, поэтому доступ только через
// accessors под RWMutex (тесты меняют их между запросами).
var (
	tokensMu     sync.RWMutex
	notifyToken  string
	adminToken   string
	historyToken string
)

// upstreamClient — один HTTP-клиент на процесс. client.New() в каждом
// обработчике создавал отдельный пул соединений, который не освобождался.
var upstreamClient = client.New()

// tokenEqual сравнивает токены за постоянное время, не раскрывая длину.
func tokenEqual(got, expected string) bool {
	if got == "" || expected == "" {
		return false
	}
	g := sha256.Sum256([]byte(got))
	e := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(g[:], e[:]) == 1
}

func setTokens(admin, notify string) {
	tokensMu.Lock()
	adminToken, notifyToken = admin, notify
	tokensMu.Unlock()
}

func currentAdminToken() string {
	tokensMu.RLock()
	defer tokensMu.RUnlock()
	return adminToken
}

func currentNotifyToken() string {
	tokensMu.RLock()
	defer tokensMu.RUnlock()
	return notifyToken
}

func setHistoryToken(t string) {
	tokensMu.Lock()
	historyToken = t
	tokensMu.Unlock()
}

func currentHistoryToken() string {
	tokensMu.RLock()
	defer tokensMu.RUnlock()
	return historyToken
}

// historyGatewayTokenFromEnv читает секрет gateway→history с fallback на
// NOTIFY_GATEWAY_TOKEN: раскатка не требует новой переменной в .env.
func historyGatewayTokenFromEnv() string {
	if t := strings.TrimSpace(platform.GetEnv("HISTORY_GATEWAY_TOKEN", "")); t != "" {
		return t
	}
	return strings.TrimSpace(platform.GetEnv("NOTIFY_GATEWAY_TOKEN", ""))
}

// loadAdminToken читает ADMIN_TOKEN из окружения. Токен НЕ генерируется на лету:
// неизвестный токен, напечатанный в лог, означает, что UI работать не будет, а
// админские ручки молча закроются. Лучше упасть на старте с понятным текстом,
// чем работать «в половинку». Сгенерировать: openssl rand -hex 32 -> ADMIN_TOKEN.
func adminTokenFromEnv(getenv func(string) string) (string, error) {
	t := strings.TrimSpace(getenv("ADMIN_TOKEN"))
	if t == "" {
		return "", errors.New("ADMIN_TOKEN не задан. Без него закрыты host-exec ручки " +
			"(/notify/update/*, /notify/env*, /scheduler/update/*). " +
			"Добавьте в .env: ADMIN_TOKEN=<openssl rand -hex 32>")
	}
	return t, nil
}

func loadAdminToken() string {
	t, err := adminTokenFromEnv(func(k string) string { return platform.GetEnv(k, "") })
	if err != nil {
		log.Fatal("❌ ", err)
	}
	return t
}

// bearerToken — RFC 7235: схема аутентификации регистронезависима.
func bearerToken(auth string) string {
	auth = strings.TrimSpace(auth)
	const prefix = "bearer "
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(prefix):])
}

// adminTokenCandidates — все места, откуда клиент может прислать ADMIN_TOKEN.
func adminTokenCandidates(c fiber.Ctx) []string {
	out := make([]string, 0, 2)
	if v := strings.TrimSpace(c.Get("X-Admin-Token")); v != "" {
		out = append(out, v)
	}
	if v := bearerToken(c.Get("Authorization")); v != "" {
		out = append(out, v)
	}
	return out
}

// adminTokenOK — сверяет ЛЮБОЙ валидный кандидат с ADMIN_TOKEN (constant time).
// Раньше приоритет отдавался X-Admin-Token, и валидный Bearer игнорировался,
// если рядом был мусорный X-Admin-Token.
func adminTokenOK(c fiber.Ctx) bool {
	expected := currentAdminToken()
	if expected == "" {
		return false
	}
	for _, got := range adminTokenCandidates(c) {
		if tokenEqual(got, expected) {
			return true
		}
	}
	return false
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

// maxProxyPathDecodes — сколько раз разворачиваем %XX. 8 покрывает любые
// реальные цепочки прокси; глубже — fail-closed.
const maxProxyPathDecodes = 8

var proxyPctEscapeRe = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// normalizeProxyPath декодирует %XX до стабилизации (а не фиксированные 3
// раза) и возвращает путь без ведущего слэша — ровно то, что уйдёт upstream.
// Любая ошибка означает «путь не разобрали»: его нельзя ни проверять, ни
// проксировать.
func normalizeProxyPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)

	for i := 0; ; i++ {
		if i >= maxProxyPathDecodes {
			return "", errors.New("too many path encoding layers")
		}
		if !proxyPctEscapeRe.MatchString(p) {
			break
		}
		u, err := url.PathUnescape(p)
		if err != nil {
			return "", errors.New("invalid path encoding")
		}
		if u == p {
			break
		}
		p = u
	}

	p = strings.ReplaceAll(p, "\\", "/")
	if strings.ContainsRune(p, '\x00') {
		return "", errors.New("invalid path")
	}
	// %3F/%23 после декодирования = подмена query/fragment на upstream.
	if strings.ContainsAny(p, "?#") {
		return "", errors.New("invalid path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errors.New("path traversal")
		}
	}

	p = pathpkg.Clean("/" + p)
	p = strings.TrimPrefix(p, "/")
	if p == "." {
		p = ""
	}
	return p, nil
}

// canonicalProxyPath возвращает "" при ошибке; adminProtectedPath трактует
// это как «защищено» (fail-closed).
func canonicalProxyPath(raw string) string {
	p, err := normalizeProxyPath(raw)
	if err != nil {
		return ""
	}
	return p
}

func validateProxyPath(raw string) error {
	_, err := normalizeProxyPath(raw)
	return err
}

// adminGuardedServices — сервисы, для которых вообще применяется
// admin-политика.
var adminGuardedServices = []string{"notify", "scheduler", "stats", "media", "history", "music"}

// adminProxyService — сервис, у которого есть закрываемые ручки.
func adminProxyService(name string) bool {
	for _, s := range adminGuardedServices {
		if s == name {
			return true
		}
	}
	return false
}

// schedulerReadOnly — единственные ручки scheduler без ADMIN_TOKEN:
// только чтение и только GET/HEAD. Всё остальное, включая новые
// host-exec/деструктивные ручки, закрывается автоматически.
func schedulerReadOnly(p, method string) bool {
	if method != fiber.MethodGet && method != fiber.MethodHead {
		return false
	}
	switch p {
	case "health", "health/services", "health/summary", "health/history",
		"health/memory", "metrics", "tasks", "backup/list", "update/status":
		return true
	}
	return false
}

// statsReadOnly — для stats любой не-GET/HEAD требует ADMIN_TOKEN.
// Сейчас это только /cleanup/run, но новые mutating-ручки закроются сами.
func statsReadOnly(_ string, method string) bool {
	return method == fiber.MethodGet || method == fiber.MethodHead
}

// mediaNeedsAdmin — операции, которые пишут на хост или запускают opener.
// Обычные POST (progress, wishlist, subscriptions) остаются доступны.
func mediaNeedsAdmin(p, method string) bool {
	if method == fiber.MethodGet || method == fiber.MethodHead {
		return false
	}
	switch {
	case p == "duplicates/resolve",
		p == "rename/apply",
		p == "photos/cache",
		p == "scan",
		strings.HasPrefix(p, "scan/"),
		p == "open" || strings.HasPrefix(p, "open/"):
		return true
	case strings.HasPrefix(p, "episode/") && strings.HasSuffix(p, "/open"):
		return true
	}
	return false
}

// adminProtectedPath — единый источник истины для middleware и proxyHandler.
// Ошибка нормализации пути трактуется как «защищено» (fail-closed).
func adminProtectedPath(service, method, path string) bool {
	if !adminProxyService(service) {
		return false
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	switch method {
	case fiber.MethodGet, fiber.MethodHead, fiber.MethodPost,
		fiber.MethodPut, fiber.MethodDelete, fiber.MethodPatch:
	default:
		// Непроксируемые методы всё равно получат 405, upstream не вызовется.
		return false
	}

	p, err := normalizeProxyPath(path)
	if err != nil {
		return true
	}
	p = strings.ToLower(p)

	switch service {
	case "notify":
		return p == "env" || strings.HasPrefix(p, "env/") ||
			p == "update" || strings.HasPrefix(p, "update/")
	case "scheduler":
		return !schedulerReadOnly(p, method)
	case "stats":
		return !statsReadOnly(p, method)
	case "media":
		return mediaNeedsAdmin(p, method)
	case "history":
		// Чтение истории открыто (requireAuth решает), удаление — только ADMIN_TOKEN.
		return method == fiber.MethodDelete
	case "music":
		// Удаление истории прослушиваний — только ADMIN_TOKEN. Остальные
		// DELETE (очередь, избранное, плейлисты) — обычные пользовательские.
		return method == fiber.MethodDelete &&
			(p == "listens" || strings.HasPrefix(p, "listens/"))
	}
	return false
}

// adminGuard проверяет политику по полному пути, а не по списку префиксов:
// новые ручки закрываются без правки списка.
func adminGuard(service string) fiber.Handler {
	return func(c fiber.Ctx) error {
		p := strings.TrimPrefix(c.Path(), "/"+service)
		p = strings.TrimPrefix(p, "/")
		if adminProtectedPath(service, c.Method(), p) {
			return adminAuth(c)
		}
		return c.Next()
	}
}

func registerAdminAuth(app *fiber.App) {
	for _, svc := range adminGuardedServices {
		app.Use("/"+svc+"/*", adminGuard(svc))
	}
}

// serviceTokenPath — ручки, куда gateway подставляет сервисный X-Gateway-Token
// БЕЗ adminAuth: /notify/send — обычные уведомления (их шлют сервисы стека:
// stats/media/torrent), host-exec там нет, и закрывать их админ-токеном нельзя.
func serviceTokenPath(service, method, path string) bool {
	if service != "notify" {
		return false
	}
	p := strings.ToLower(canonicalProxyPath(path))
	return p == "send" && (method == fiber.MethodPost || method == fiber.MethodPut)
}

// requireAuthMiddleware проверяет JWT. Разрешён только HS256: без явной
// проверки алгоритма alg=none и чужие схемы ломают аутентификацию.
func requireAuthMiddleware(jwtSecret []byte, authEnabled bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		if !authEnabled {
			return c.Next()
		}
		// Валидный ADMIN_TOKEN — самостоятельный доступ: он идёт в том же
		// заголовке Authorization, что и JWT, и в админ-вызовах UI заменяет его.
		if adminTokenOK(c) {
			return c.Next()
		}

		auth := c.Get("Authorization")
		tokenStr := bearerToken(auth)
		if tokenStr == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "no token"})
		}

		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		},
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithExpirationRequired(),
			// auth-service выпускает токены только с этим issuer и scope=admin.
			jwt.WithIssuer("nas-auth"),
			jwt.WithLeeway(30*time.Second),
		)
		if err != nil || !token.Valid {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
		}
		if scope, _ := claims["scope"].(string); scope != "admin" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
		}
		return c.Next()
	}
}

// authServiceProxy проксирует запрос в auth-service, сохраняя Authorization
// и передавая реальный IP клиента (auth-service rate-limit'ит по XFF).
func authServiceProxy(baseURL string, pathFn func(fiber.Ctx) string) fiber.Handler {
	return func(c fiber.Ctx) error {
		headers := map[string]string{
			"Content-Type":    "application/json",
			"X-Forwarded-For": c.IP(),
		}
		if a := c.Get("Authorization"); a != "" {
			headers["Authorization"] = a
		}

		cfg := client.Config{Timeout: 10 * time.Second, Header: headers, Ctx: c.Context()}

		var (
			resp *client.Response
			err  error
		)
		switch c.Method() {
		case fiber.MethodGet:
			resp, err = upstreamClient.Get(baseURL+pathFn(c), cfg)
		case fiber.MethodPost:
			cfg.Body = c.Body()
			resp, err = upstreamClient.Post(baseURL+pathFn(c), cfg)
		default:
			return c.Status(fiber.StatusMethodNotAllowed).JSON(fiber.Map{"error": "method not allowed"})
		}
		if err != nil {
			log.Printf("auth proxy %s: %v", pathFn(c), err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "auth-service недоступен"})
		}
		defer resp.Close()

		if ct := sanitizeProxyHeader(resp.Header("Content-Type")); ct != "" {
			c.Set("Content-Type", ct)
		}
		c.Set("Cache-Control", "no-store")
		c.Set("X-Content-Type-Options", "nosniff")
		// Копируем тело: Send() использует SetBodyRaw, а Close() возвращает
		// буфер в пул клиента.
		body := append([]byte(nil), resp.Body()...)
		return c.Status(resp.StatusCode()).Send(body)
	}
}

// sanitizeImageContentType не пропускает произвольный Content-Type upstream:
// text/html с этого роута браузер иначе отрендерит как HTML.
func sanitizeImageContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch ct {
	case "image/jpeg", "image/jpg", "image/png", "image/webp", "image/gif", "image/avif":
		return ct
	default:
		return "image/jpeg"
	}
}

// sanitizeProxyHeader не пропускает CR/LF/NUL из заголовков upstream:
// иначе ответ сервиса мог бы инжектировать заголовки в ответ gateway.
func sanitizeProxyHeader(v string) string {
	if strings.ContainsAny(v, "\r\n\x00") {
		return ""
	}
	return v
}

// hopByHopRequestHeaders не проксируются в upstream (RFC 7230 §6.1), а
// служебные заголовки gateway/client отбрасываются всегда. Cookie и
// method/URL-override не даём влиять на upstream: gateway авторизует только
// по Authorization/JWT, а override превращает GET в POST на стороне сервиса.
var hopByHopRequestHeaders = map[string]struct{}{
	"Connection":             {},
	"Keep-Alive":             {},
	"Proxy-Authenticate":     {},
	"Proxy-Authorization":    {},
	"Proxy-Connection":       {},
	"Te":                     {},
	"Trailer":                {},
	"Transfer-Encoding":      {},
	"Upgrade":                {},
	"Content-Length":         {},
	"Host":                   {},
	"Authorization":          {},
	"X-Gateway-Token":        {},
	"X-Admin-Token":          {},
	"Cookie":                 {},
	"X-Http-Method-Override": {},
	"X-Method-Override":      {},
	"X-Original-Url":         {},
	"X-Rewrite-Url":          {},
}

// connectionTokens — заголовки, перечисленные в Connection, тоже hop-by-hop.
func connectionTokens(c fiber.Ctx) map[string]struct{} {
	out := map[string]struct{}{}
	c.Request().Header.VisitAll(func(k, v []byte) {
		if !strings.EqualFold(string(k), "Connection") {
			return
		}
		for _, tok := range strings.Split(string(v), ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out[textproto.CanonicalMIMEHeaderKey(tok)] = struct{}{}
			}
		}
	})
	return out
}

func copyRequestHeaders(dst map[string]string, c fiber.Ctx) {
	nominated := connectionTokens(c)
	c.Request().Header.VisitAll(func(k, v []byte) {
		key := textproto.CanonicalMIMEHeaderKey(string(k))
		if _, skip := hopByHopRequestHeaders[key]; skip {
			return
		}
		if _, skip := nominated[key]; skip {
			return
		}
		lk := strings.ToLower(key)
		if strings.HasPrefix(lk, "x-forwarded-") || lk == "forwarded" || lk == "x-real-ip" {
			return
		}
		dst[key] = string(v)
	})

	// Доверенные forwarding-заголовки ставит только gateway.
	dst["X-Forwarded-For"] = c.IP()
	dst["X-Forwarded-Proto"] = c.Protocol()
}

// sanitizeRedirect не отдаёт браузеру open redirect на чужой origin.
func sanitizeRedirect(c fiber.Ctx, loc string) string {
	loc = sanitizeProxyHeader(loc)
	if loc == "" || strings.HasPrefix(loc, "//") {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	// magnet — не web-origin: open redirect на чужой сайт через него
	// невозможен. Нужен для прокси скачивания Prowlarr (302 → magnet).
	if strings.EqualFold(u.Scheme, "magnet") {
		return loc
	}
	if u.Scheme != "" || u.Host != "" {
		if (u.Scheme != "http" && u.Scheme != "https") ||
			!strings.EqualFold(u.Host, c.Host()) {
			return ""
		}
	}
	return loc
}

// sanitizeProxyContentType не даёт upstream отдать активный контент
// (HTML/XHTML/SVG/XML) с origin gateway. Вместе с nosniff и CSP это
// исключает исполнение ответа сервиса в контексте дашборда.
func sanitizeProxyContentType(ct string) string {
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch mt {
	case "text/html", "application/xhtml+xml", "image/svg+xml",
		"text/xml", "application/xml":
		return "text/plain; charset=utf-8"
	}
	return ct
}

// applyUpstreamHeaders пробрасывает только безопасные заголовки и добавляет
// периметровую защиту от XSS/open redirect. Set-Cookie намеренно не
// пробрасывается: иначе upstream может ставить cookie на origin gateway.
func applyUpstreamHeaders(c fiber.Ctx, resp *client.Response) {
	for _, h := range []string{
		"Content-Type", "Cache-Control", "Location", "Content-Disposition",
		"Accept-Ranges", "Content-Range", "ETag", "Last-Modified",
		"WWW-Authenticate", "Retry-After",
	} {
		v := sanitizeProxyHeader(resp.Header(h))
		if v == "" {
			continue
		}
		switch h {
		case "Content-Type":
			v = sanitizeProxyContentType(v)
		case "Location":
			v = sanitizeRedirect(c, v)
			if v == "" {
				continue
			}
		}
		c.Set(h, v)
	}

	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Referrer-Policy", "no-referrer")
	c.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
}

const maxCoverBytes = 10 << 20

// validateCoverURL — периметровая проверка SSRF. DNS/redirect-проверка
// остаётся в books-service; здесь закрываем literal loopback/private/metadata.
func validateCoverURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("url is required")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("invalid url: only http/https allowed")
	}
	if u.User != nil {
		return errors.New("invalid url")
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || host == "localhost" || host == "metadata.google.internal" ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") {
		return errors.New("invalid url")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("invalid url")
		}
	}
	return nil
}

func main() {
	app := fiber.New(fiber.Config{
		AppName:      "NAS Gateway",
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  60 * time.Second,
		BodyLimit:    16 << 20,
	})
	platform.StartPprof()
	// metrics снаружи, recover внутри: паника превращается в err, и metrics
	// фиксирует 500, а не статус по умолчанию 200.
	app.Use(metricsMiddleware())
	app.Use(recover.New())
	app.Use(logger.New())

	// /metrics не публикуется на :8080: отдельный listener :9091 доступен
	// только внутри docker-сети (prometheus.yml).
	metricsApp := fiber.New(fiber.Config{
		AppName:      "NAS Gateway Metrics",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	})
	metricsApp.Get("/metrics", adaptor.HTTPHandler(metricsHandler()))
	go func() {
		addr := platform.GetEnv("METRICS_ADDR", ":9091")
		if err := metricsApp.Listen(addr); err != nil {
			log.Printf("metrics listener %s: %v", addr, err)
		}
	}()

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
	authEnabled, err := strconv.ParseBool(strings.TrimSpace(platform.GetEnv("AUTH_ENABLED", "false")))
	if err != nil {
		log.Fatalf("❌ AUTH_ENABLED=%q: ожидается true/false", platform.GetEnv("AUTH_ENABLED", ""))
	}
	if authEnabled && (len(jwtSecret) < 32 || string(jwtSecret) == "change-me-in-production") {
		log.Fatal("❌ AUTH_ENABLED=true, но JWT_SECRET пуст/короче 32 байт/дефолтный (openssl rand -hex 32)")
	}

	// Секрет для доступа к notify-service host-exec ручкам (/update/*, /env).
	notifyTok := strings.TrimSpace(platform.GetEnv("NOTIFY_GATEWAY_TOKEN", ""))
	if notifyTok == "" {
		log.Println("⚠️ NOTIFY_GATEWAY_TOKEN не задан — /notify/update/* и /notify/env вернут 403")
	}
	setTokens(loadAdminToken(), notifyTok)
	setHistoryToken(historyGatewayTokenFromEnv())
	if currentHistoryToken() == "" {
		log.Println("⚠️ HISTORY_GATEWAY_TOKEN/NOTIFY_GATEWAY_TOKEN не задан — history-service вернёт 503 на все ручки кроме /health")
	}

	// ============================================================
	// ПУБЛИЧНЫЕ РОУТЫ
	// ============================================================
	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("gateway", nil)) })

	app.Post("/auth/login", authServiceProxy(authURL, func(fiber.Ctx) string {
		return "/login"
	}))
	app.Get("/auth/verify", authServiceProxy(authURL, func(fiber.Ctx) string {
		return "/verify"
	}))

	// ============================================================
	// JWT MIDDLEWARE (опциональный)
	// ============================================================
	requireAuth := requireAuthMiddleware(jwtSecret, authEnabled)

	// Профили: единственный публичный вход в auth-service (порт 8086 наружу
	// не публикуется). requireAuth отсекает анонимов при AUTH_ENABLED=true,
	// auth-service дополнительно проверяет scope=admin.
	app.Get("/auth/profiles", requireAuth, authServiceProxy(authURL, func(fiber.Ctx) string {
		return "/profiles"
	}))
	app.Post("/auth/profiles", requireAuth, authServiceProxy(authURL, func(fiber.Ctx) string {
		return "/profiles"
	}))
	app.Post("/auth/profiles/:id/select", requireAuth, authServiceProxy(authURL, func(c fiber.Ctx) string {
		return "/profiles/" + url.PathEscape(c.Params("id")) + "/select"
	}))

	// ============================================================
	// ПРОКСИОБЛОЖКИ (публичный, без auth)
	// ============================================================
	app.Get("/books/cover", func(c fiber.Ctx) error {
		coverURL := strings.TrimSpace(c.Query("url"))
		if err := validateCoverURL(coverURL); err != nil {
			return c.Status(fiber.StatusBadRequest).SendString(err.Error())
		}

		resp, err := upstreamClient.Get(booksURL+"/cover?url="+url.QueryEscape(coverURL), client.Config{
			Timeout: 15 * time.Second,
			Ctx:     c.Context(),
		})
		if err != nil {
			log.Printf("books/cover: %v", err)
			return c.Status(fiber.StatusBadGateway).SendString("cover service unavailable")
		}
		defer resp.Close()

		if resp.StatusCode() != fiber.StatusOK {
			log.Printf("books/cover: upstream status %d", resp.StatusCode())
			return c.Status(resp.StatusCode()).SendString("upstream error")
		}

		body, err := io.ReadAll(io.LimitReader(resp.BodyStream(), maxCoverBytes+1))
		if err != nil {
			return c.Status(fiber.StatusBadGateway).SendString("cover read error")
		}
		if len(body) > maxCoverBytes {
			return c.Status(fiber.StatusBadGateway).SendString("cover too large")
		}

		c.Set("Content-Type", sanitizeImageContentType(resp.Header("Content-Type")))
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
		c.Set("Cache-Control", "private, max-age=86400")
		return c.Send(body)
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
	// /search/* не матчит /search-all — отдельный Use обязателен.
	app.Use("/search-all", requireAuth)
	app.Use("/youtube/*", requireAuth)
	app.Use("/media/*", requireAuth)
	app.Use("/music/*", requireAuth)

	// ADMIN_TOKEN на всё, что даёт host-exec или доступ к .env хоста.
	// Политика одна для middleware и proxyHandler: новые mutating-ручки
	// scheduler/stats/media закрываются автоматически (default-deny).
	registerAdminAuth(app)

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
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()

		result := fiber.Map{}
		var mu sync.Mutex
		var wg sync.WaitGroup

		for name, svcURL := range services {
			wg.Add(1)
			go func(name, svcURL string) {
				defer wg.Done()

				resp, err := upstreamClient.Get(svcURL+"/health", client.Config{
					Timeout: 2 * time.Second,
					Ctx:     ctx,
				})

				mu.Lock()
				defer mu.Unlock()

				if err != nil {
					result[name] = "down"
					return
				}
				defer resp.Close()
				if resp.StatusCode() != fiber.StatusOK {
					result[name] = "down"
					return
				}
				result[name] = string(resp.Body())
			}(name, svcURL)
		}
		wg.Wait()
		return c.JSON(result)
	})

	// Умный поиск: парсим фразу → параметры → media-service /movies/smart.
	// Регистрируется до catch-all /search/*.
	app.Post("/search/smart", func(c fiber.Ctx) error {
		var req struct {
			Query string `json:"query"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad json"})
		}
		if strings.TrimSpace(req.Query) == "" {
			return c.JSON(fiber.Map{"movies": []fiber.Map{}, "count": 0, "params": fiber.Map{}})
		}
		if len(req.Query) > smartQueryMaxBytes {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "query too long"})
		}
		params := parseSmartQuery(req.Query)
		if len(params) == 0 {
			return c.JSON(fiber.Map{"movies": []fiber.Map{}, "count": 0, "params": params})
		}
		body, err := json.Marshal(params)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		}
		resp, err := upstreamClient.Post(mediaURL+"/movies/smart", client.Config{
			Timeout: 15 * time.Second,
			Header:  map[string]string{"Content-Type": "application/json"},
			Body:    body,
			Ctx:     c.Context(),
		})
		if err != nil {
			log.Printf("search/smart: media-service: %v", err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "media-service unavailable"})
		}
		defer resp.Close()
		respBody := append([]byte(nil), resp.Body()...)
		return c.Status(resp.StatusCode()).Send(respBody)
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
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "q is required"})
		}
		query := url.QueryEscape(q)

		ctx, cancel := context.WithTimeout(c.Context(), 20*time.Second)
		defer cancel()

		type job struct {
			key, target, field string
		}
		jobs := []job{
			{"movies", mediaURL + "/movies?q=" + query + "&limit=20", "items"},
			{"tv", mediaURL + "/tv?q=" + query + "&limit=20", "items"},
			{"torrents", searchURL + "/search?q=" + query, "results"},
			{"books", booksURL + "/search?q=" + query, "books"},
		}

		result := fiber.Map{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		wg.Add(len(jobs))

		fetch := func(j job) {
			defer wg.Done()
			resp, err := upstreamClient.Get(j.target, client.Config{
				Timeout: 30 * time.Second,
				Ctx:     ctx,
			})
			if err != nil {
				return
			}
			defer resp.Close()

			var parsed map[string]interface{}
			if json.Unmarshal(resp.Body(), &parsed) == nil {
				mu.Lock()
				result[j.key] = parsed[j.field]
				mu.Unlock()
			}
		}

		for _, j := range jobs {
			go fetch(j)
		}
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
	base, baseErr := url.Parse(baseURL)
	if baseErr != nil || base.Scheme == "" || base.Host == "" {
		log.Printf("proxy %s: invalid base URL %q: %v", name, baseURL, baseErr)
		return func(c fiber.Ctx) error {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
				"error":   "service misconfigured",
				"service": name,
			})
		}
	}

	return func(c fiber.Ctx) error {
		proxyPath := c.Params("*")

		normPath, err := normalizeProxyPath(proxyPath)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid path"})
		}

		// Defense in depth: middleware может не совпасть по семантике префиксов,
		// но host-exec запрос не уйдёт дальше.
		needsAdmin := adminProxyService(name) &&
			adminProtectedPath(name, c.Method(), proxyPath)
		if needsAdmin && !adminTokenOK(c) {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
				"error": "admin token required: send 'Authorization: Bearer <ADMIN_TOKEN>'",
			})
		}

		// target строится из того же нормализованного пути, который проверяли.
		targetURL := *base
		targetURL.Path = pathpkg.Join("/", base.Path, normPath)
		targetURL.RawPath = ""
		targetURL.RawQuery = string(c.Request().URI().QueryString())

		timeout := 30 * time.Second
		switch name {
		case "stats":
			timeout = 60 * time.Second // /stats/nas: du -sh по CIFS может быть >30s
		case "search":
			timeout = 60 * time.Second // поиск Prowlarr и скачивание .torrent — до 60s
		}

		cfg := client.Config{
			Timeout: timeout,
			Ctx:     c.Context(),
			Header:  map[string]string{},
		}
		copyRequestHeaders(cfg.Header, c)

		// Внутренние служебные заголовки ставятся ПОСЛЕ копирования клиентских —
		// иначе клиент мог бы подсунуть свой X-Gateway-Token/X-Admin-Token.
		notifyTok, adminTok := currentNotifyToken(), currentAdminToken()

		// history-service требует сервисный токен на всех путях (обход gateway
		// закрыт), поэтому подставляем его и для чтения, и для DELETE.
		if name == "history" {
			if tok := currentHistoryToken(); tok != "" {
				cfg.Header["X-Gateway-Token"] = tok
			}
		}

		switch {
		case needsAdmin:
			if name == "notify" && notifyTok != "" {
				cfg.Header["X-Gateway-Token"] = notifyTok
			}
			// notify-service проверяет только X-Gateway-Token; мастер-ADMIN_TOKEN
			// ему не нужен. X-Admin-Token получают scheduler, history и media:
			// media-service дополнительно проверяет его на деструктивных ручках.
			if (name == "scheduler" || name == "history" || name == "media") && adminTok != "" {
				cfg.Header["X-Admin-Token"] = adminTok
			}
		case serviceTokenPath(name, c.Method(), proxyPath):
			// /notify/send: только сервисный токен, adminAuth не требуется.
			if notifyTok != "" {
				cfg.Header["X-Gateway-Token"] = notifyTok
			}
		}

		if body := c.Body(); len(body) > 0 {
			cfg.Body = body
		}

		var resp *client.Response
		switch c.Method() {
		case fiber.MethodGet:
			resp, err = upstreamClient.Get(targetURL.String(), cfg)
		case fiber.MethodHead:
			resp, err = upstreamClient.Head(targetURL.String(), cfg)
		case fiber.MethodPost:
			resp, err = upstreamClient.Post(targetURL.String(), cfg)
		case fiber.MethodPut:
			resp, err = upstreamClient.Put(targetURL.String(), cfg)
		case fiber.MethodDelete:
			resp, err = upstreamClient.Delete(targetURL.String(), cfg)
		case fiber.MethodPatch:
			resp, err = upstreamClient.Patch(targetURL.String(), cfg)
		default:
			return c.Status(fiber.StatusMethodNotAllowed).SendString("Method Not Allowed")
		}

		if err != nil {
			log.Printf("proxy %s %s: %v", name, targetURL.Path, err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
				"error":   "service unavailable",
				"service": name,
			})
		}
		defer resp.Close()

		applyUpstreamHeaders(c, resp)

		// Копия ДО Close(): Send() использует SetBodyRaw, а Close() возвращает
		// буфер в пул клиента.
		body := append([]byte(nil), resp.Body()...)
		if c.Method() == fiber.MethodHead {
			return c.SendStatus(resp.StatusCode())
		}
		return c.Status(resp.StatusCode()).Send(body)
	}
}

// smartGenres — упорядоченный список жанровых триггеров. Порядок важен:
// обход map в Go рандомизирован, из-за чего "мелодрама" (содержит и "драма",
// и "мелодрам") давала случайный жанр. Более специфичные ключи идут раньше.
var smartGenres = []struct{ keyword, genre string }{
	{"мелодрам", "мелодрам"}, {"romance", "мелодрам"},
	{"комеди", "комеди"}, {"comedy", "комеди"},
	{"боевик", "боевик"}, {"action", "боевик"},
	{"драма", "драм"}, {"drama", "драм"},
	{"фантаст", "фантаст"}, {"sci-fi", "фантаст"},
	{"триллер", "триллер"}, {"thriller", "триллер"},
	{"ужас", "ужас"}, {"horror", "ужас"},
	{"анимац", "анимац"}, {"мульт", "анимац"},
	{"детектив", "детектив"}, {"mystery", "детектив"},
	{"криминал", "криминал"}, {"crime", "криминал"},
}

var (
	smartMinutesRe = regexp.MustCompile(`(\d+)\s*мин`)
	// \b не даёт вытащить "2010" из "20100".
	smartYearRe = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
)

const (
	smartMaxRuntime = 24 * 60 // минуты
	smartLimit      = 50
	// /search/smart прогоняет строку через regexp/strings.Contains —
	// без лимита это дешёвый CPU-DoS (BodyLimit допускает мегабайты).
	smartQueryMaxBytes = 1024
)

// parseSmartQuery превращает свободную фразу в параметры поиска фильмов.
func parseSmartQuery(q string) map[string]interface{} {
	q = strings.ToLower(q)
	p := map[string]interface{}{}
	for _, g := range smartGenres {
		if strings.Contains(q, g.keyword) {
			p["genre"] = g.genre
			break
		}
	}

	// Явно указанные минуты приоритетнее слов «короткий»/«длинный».
	explicitRuntime := false
	if m := smartMinutesRe.FindStringSubmatch(q); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			p["runtime_max"] = min(n+10, smartMaxRuntime)
			explicitRuntime = true
		}
	}
	if !explicitRuntime {
		if strings.Contains(q, "коротк") || strings.Contains(q, "быстр") {
			p["runtime_max"] = 90
		}
		if strings.Contains(q, "длинн") {
			p["runtime_min"] = 150
		}
	}
	// Противоречивый диапазон («короткий длинный фильм») не отправляем.
	if min, ok := p["runtime_min"].(int); ok {
		if max, ok := p["runtime_max"].(int); ok && max <= min {
			delete(p, "runtime_min")
			delete(p, "runtime_max")
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
	if y := smartYearRe.FindString(q); y != "" {
		if n, err := strconv.Atoi(y); err == nil && n > 0 {
			p["year_from"] = n
			p["year_to"] = n
		}
	}
	p["limit"] = smartLimit
	return p
}
