package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ============================================================
// КОНФИГ
// ============================================================
var (
	sourceURL = platform.GetEnv("FLIBUSTA_URL", "https://flibusta.is")
	booksDir  = platform.GetEnv("BOOKS_DIR", "/books")
)

const (
	maxBookBytes  = 128 << 20
	maxCoverBytes = 8 << 20
	maxFeedBytes  = 16 << 20
	maxFB2Bytes   = 128 << 20
)

var (
	errTooLarge = errors.New("response too large")
	errConflict = errors.New("file already exists")

	errRedirectBlocked  = errors.New("books-service: redirect to disallowed host")
	errTooManyRedirects = errors.New("books-service: too many redirects")

	// lookupIPAddr — точка инъекции резолвера: тесты проверяют DNS-rebinding
	// и приватные ответы без реального DNS.
	lookupIPAddr = net.DefaultResolver.LookupIPAddr

	reYear = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2})?)?$`)
	reLang = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})?$`)
	reTags = regexp.MustCompile(`(?s)<[^>]*>`)
)

// readLimited читает не больше max+1 байт, чтобы отличить «ровно max» от
// переполнения и не выесть память ответом зеркала.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errTooLarge
	}
	return b, nil
}

// ============================================================
// СТРУКТУРЫ OPDS
// ============================================================
type Feed struct {
	XMLName xml.Name `xml:"feed"`
	Entries []Entry  `xml:"entry"`
}

type Entry struct {
	Title    string   `xml:"title"`
	ID       string   `xml:"id"`
	Authors  []Author `xml:"author"`
	Links    []Link   `xml:"link"`
	Issued   string   `xml:"issued"`
	Language string   `xml:"language"`
	Format   string   `xml:"format"`
	Content  string   `xml:"content"`
}

type Author struct {
	Name string `xml:"name"`
	URI  string `xml:"uri"`
}

type Link struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr"`
}

// ============================================================
// MAIN
// ============================================================
// newApp собирает роутер без запуска listener: тесты обязаны проверять
// продовые обработчики, а не их копии.
func newApp() *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "books-service",
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
		BodyLimit:    1 << 20,
	})
	app.Use(recover.New())
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error {
		// /health — liveness: не ходим в сеть, отдаём только кэш.
		src, available := cachedMirror()
		return c.JSON(platform.HealthJSON("books", fiber.Map{
			"source":           redactURL(src),
			"source_available": available,
		}))
	})

	app.Get("/search", func(c fiber.Ctx) error {
		query := strings.TrimSpace(c.Query("q"))
		if query == "" || utf8.RuneCountInString(query) > 200 {
			return c.Status(400).JSON(fiber.Map{"error": "query is required"})
		}

		books, err := searchBooks(query)
		if err != nil {
			log.Printf("search %q: %v", query, err)
			// Не отдаём клиенту внутренние адреса/текст ошибки.
			return c.Status(503).JSON(fiber.Map{
				"error":    "Флибуста временно недоступна. Все зеркала не отвечают.",
				"hint":     "Можно развернуть локальное зеркало (профиль flibusta-local) и не зависеть от серверов Флибусты.",
				"docs_url": "https://github.com/FuzzyHD1993/nas-stack/blob/main/docs/local-flibusta.md",
			})
		}

		return c.JSON(fiber.Map{
			"query": query,
			"count": len(books),
			"books": books,
		})
	})

	// GET /read/:id — локальный файл или прокси книги с зеркала (или онлайн).
	// Отдаёт .fb2 как файл: браузер не умеет открывать FB2 (это XML).
	app.Get("/read/:id", func(c fiber.Ctx) error {
		id := c.Params("id")

		// 1) Локально скачанный файл: только безопасное имя внутри booksDir,
		// без симлинков наружу.
		if path, err := resolveStoredBook(id); err == nil {
			return sendBookFile(c, path)
		}

		// 2) Онлайн-зеркала.
		hc := makeClient()
		for _, base := range orderedSources() {
			u := readURL(base, id)
			if u == "" || !isSafeURL(u) {
				continue
			}
			req, err := newRequestCtx(c.Context(), http.MethodGet, u)
			if err != nil {
				continue
			}
			resp, err := hc.Do(req)
			if err != nil {
				continue
			}
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				continue
			}

			body, err := readLimited(resp.Body, maxBookBytes)
			_ = resp.Body.Close()
			if err != nil {
				log.Printf("read: %s: %v", redactURL(base), err)
				continue
			}

			trimmed := bytes.TrimPrefix(bytes.TrimSpace(body), []byte("\xEF\xBB\xBF"))
			if !bytes.HasPrefix(trimmed, []byte("<?xml")) &&
				!bytes.HasPrefix(trimmed, []byte("<FictionBook")) {
				continue // заглушка/HTML/zip — пробуем следующее зеркало
			}

			c.Set("Content-Type", "application/fb2+xml; charset=utf-8")
			c.Set("X-Content-Type-Options", "nosniff")
			c.Set("Content-Security-Policy", "default-src 'none'; sandbox")
			c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "book-"+safeID(id)+".fb2"))
			return c.Send(body)
		}
		return c.Status(502).JSON(fiber.Map{"error": "book not available on flibusta sources"})
	})

	app.Post("/download", func(c fiber.Ctx) error {
		var body struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Format string `json:"format"`
		}
		if err := c.Bind().JSON(&body); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		if body.ID == "" || body.Format == "" {
			return c.Status(400).JSON(fiber.Map{"error": "id and format are required"})
		}

		if err := downloadBookContext(c.Context(), body.ID, body.Title, body.Format); err != nil {
			switch {
			case errors.Is(err, errBadRequest):
				return c.Status(400).JSON(fiber.Map{"error": err.Error()})
			case errors.Is(err, errConflict):
				return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "file already exists"})
			case errors.Is(err, errTooLarge):
				return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "book is too large"})
			default:
				log.Printf("download: %v", err)
				return c.Status(502).JSON(fiber.Map{"error": "download failed"})
			}
		}

		return c.JSON(fiber.Map{"status": "downloaded", "title": body.Title})
	})

	app.Get("/downloaded", func(c fiber.Ctx) error {
		files, err := os.ReadDir(booksDir)
		if err != nil {
			log.Printf("downloaded: %v", err)
			return c.Status(500).JSON(fiber.Map{"error": "internal error"})
		}

		books := []fiber.Map{}
		for _, f := range files {
			if f.IsDir() || strings.HasPrefix(f.Name(), ".") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				// файл удалили между ReadDir и Info — не паникуем (nil-deref).
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			books = append(books, fiber.Map{
				"name":    f.Name(),
				"size":    info.Size(),
				"modtime": info.ModTime(),
			})
		}

		return c.JSON(fiber.Map{"count": len(books), "books": books})
	})

	// Отдача файла книги для читалки (EPUB/FB2) с защитой от path traversal

	// Проксирование обложки
	app.Get("/cover", func(c fiber.Ctx) error {
		coverURL := c.Query("url")
		if coverURL == "" {
			return c.Status(400).SendString("url is required")
		}
		if !isSafeURL(coverURL) {
			// SSRF: /cover?url=http://gateway:8080/health или
			// /cover?url=http://169.254.169.254/latest/meta-data/ не пройдут.
			return c.Status(400).JSON(fiber.Map{"error": "url is not allowed"})
		}

		client := makeClient()
		req, err := newRequest("GET", coverURL)
		if err != nil {
			log.Printf("cover: %v", err)
			return c.Status(500).SendString("internal error")
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("cover upstream: %v", err)
			return c.Status(502).SendString("upstream unavailable")
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != 200 {
			return c.Status(resp.StatusCode).SendString("upstream error")
		}

		contentType := strings.ToLower(strings.TrimSpace(
			strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
		switch contentType {
		case "image/jpeg", "image/jpg", "image/png", "image/webp", "image/gif", "image/avif":
		case "text/html", "application/xhtml+xml":
			return c.Status(fiber.StatusBadGateway).SendString("upstream is not an image")
		default:
			contentType = "image/jpeg"
		}

		body, err := readLimited(resp.Body, maxCoverBytes)
		if err != nil {
			log.Printf("cover: read: %v", err)
			return c.Status(fiber.StatusBadGateway).SendString("upstream error")
		}
		c.Set("Content-Type", contentType)
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Cache-Control", "public, max-age=86400")
		return c.Send(body)
	})

	return app
}

func main() {
	if err := os.MkdirAll(booksDir, 0o755); err != nil {
		log.Fatalf("❌ cannot create books dir %s: %v", booksDir, err)
	}
	platform.StartPprof()
	app := newApp()

	listenErr := make(chan error, 1)

	// Прогрев кэша зеркал — в фоне: недоступная сеть не задерживает старт.
	go func() {
		if base, ok := flibustaBaseOK(); ok {
			log.Printf("📚 books-service: активное зеркало %s", redactURL(base))
		} else {
			log.Printf("⚠️ books-service: зеркала недоступны, будет использован %s", redactURL(sourceURL))
		}
	}()

	go func() {
		log.Printf("📚 books-service на :8087")
		listenErr <- app.Listen(":8087", fiber.ListenConfig{DisableStartupMessage: true})
	}()

	sig, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-sig.Done():
	case err := <-listenErr:
		if err != nil {
			log.Printf("listen: %v", err)
		}
	}
	if err := app.ShutdownWithTimeout(15 * time.Second); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// ============================================================
// ЗАЩИТА ОТ SSRF
// ============================================================
// Ручки /cover и /read/:id принимают URL от клиента. Без проверки это
// классический SSRF: /cover?url=http://gateway:8080/health тянет
// внутренний сервис, а /cover?url=http://169.254.169.254/... — метаданные
// облака. Исходящий HTTP разрешён только для заранее известных хостов.

// allowedFetchDomains — публичные домены Флибусты.
var allowedFetchDomains = []string{
	"flibusta.is",
	"flibusta.net",
	"flibusta.lib",
	"flibusta.me",
}

// blockedHostNames — имена, которые нельзя резолвить ни при каких условиях.
var blockedHostNames = map[string]bool{
	"localhost":                true,
	"metadata":                 true,
	"metadata.google.internal": true,
	"metadata.goog":            true,
	"instance-data":            true,
}

// isBlockedIP — адрес, куда ходить нельзя: loopback, RFC1918, CGNAT,
// link-local (в т.ч. 169.254.169.254 — метаданные облака), ULA fc00::/7,
// unspecified, multicast, а также IPv4, спрятанный внутри IPv6
// (IPv4-mapped, NAT64 64:ff9b::/96, 6to4 2002::/16).
func isBlockedIP(ip net.IP) bool {
	if len(ip) == 0 {
		return true // nil и пустой net.IP{} одинаково небезопасны
	}

	if v4 := ip.To4(); v4 != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
			ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return true
		}
		// 100.64.0.0/10 — CGNAT (часто используется для внутренних сервисов).
		return v4[0] == 100 && v4[1]&0xc0 == 64
	}

	if len(ip) != net.IPv6len {
		return true // битый/неполный адрес — fail-closed
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip[0]&0xfe == 0xfc { // fc00::/7 — Unique Local Address
		return true
	}
	// NAT64 64:ff9b::/96: проверяем встроенный IPv4.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b &&
		ip[4] == 0 && ip[5] == 0 && ip[6] == 0 && ip[7] == 0 &&
		ip[8] == 0 && ip[9] == 0 && ip[10] == 0 && ip[11] == 0 {
		return isBlockedIP(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
	}
	// 6to4 2002::/16: проверяем встроенный IPv4.
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return isBlockedIP(net.IPv4(ip[2], ip[3], ip[4], ip[5]))
	}
	return false
}

// operatorHosts — точные хосты из конфигурации оператора: зеркала,
// локальное зеркало и основной источник. Это НЕ пользовательский ввод,
// поэтому берутся как есть: host.containers.internal резолвится в 169.254.x
// (link-local) и проверка «resolved IP публичный» его бы отвергла.
func operatorHosts() map[string]bool {
	out := map[string]bool{}
	add := func(raw string) {
		if h := hostOf(raw); h != "" {
			out[h] = true
		}
	}
	for _, m := range getFlibustaMirrors() {
		add(m)
	}
	add(platform.GetEnv("FLIBUSTA_LOCAL_URL", ""))
	add(sourceURL)
	return out
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// operatorTargets — точные пары host:port из конфигурации оператора.
// Порт — часть политики: локальное зеркало не открывает все порты хоста.
func operatorTargets() map[string]bool {
	out := map[string]bool{}
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return
		}
		port := u.Port()
		if port == "" {
			port = defaultPort(u.Scheme)
		}
		if port == "" {
			return
		}
		out[net.JoinHostPort(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port)] = true
	}
	for _, m := range getFlibustaMirrors() {
		add(m)
	}
	add(platform.GetEnv("FLIBUSTA_LOCAL_URL", ""))
	add(sourceURL)
	return out
}

// domainAllowed — домен из allowlist Флибусты (или его поддомен).
func domainAllowed(host string) bool {
	for _, d := range allowedFetchDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// DNS-кэш: резолвим каждый хост не чаще раза в 5 минут. Отрицательный
// ответ кэшируем коротко — иначе уникальный поддомен генерирует DNS-запрос
// на каждый HTTP-вызов.
type dnsVerdict struct {
	public  bool
	expires time.Time
}

var (
	dnsMu    sync.Mutex
	dnsCache = map[string]dnsVerdict{}
)

// hostResolvesPublic — резолвит домен и проверяет, что ВСЕ адреса публичные.
// Любой private/loopback/link-local адрес в ответе DNS → отказ (защита и от
// подмены DNS, и от DNS-rebinding).
func hostResolvesPublic(host string) bool {
	const (
		positiveTTL = 5 * time.Minute
		negativeTTL = 30 * time.Second
	)
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	dnsMu.Lock()
	if v, ok := dnsCache[host]; ok && time.Now().Before(v.expires) {
		dnsMu.Unlock()
		return v.public
	}
	dnsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := lookupIPAddr(ctx, host)
	public := err == nil && len(ips) > 0
	if public {
		for _, ip := range ips {
			if isBlockedIP(ip.IP) {
				public = false
				break
			}
		}
	}

	ttl := positiveTTL
	if !public {
		ttl = negativeTTL
	}

	dnsMu.Lock()
	if len(dnsCache) > 1024 { // без eviction кэш — вектор роста памяти
		dnsCache = map[string]dnsVerdict{}
	}
	dnsCache[host] = dnsVerdict{public: public, expires: time.Now().Add(ttl)}
	dnsMu.Unlock()
	return public
}

// isSafeURL — можно ли делать исходящий HTTP-запрос на этот URL.
func isSafeURL(rawURL string) bool {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
	default:
		// file://, gopher://, ftp:// и прочие схемы не поддерживаем.
		return false
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || blockedHostNames[host] || strings.HasSuffix(host, ".localhost") {
		return false
	}

	port := u.Port()
	if port == "" {
		port = defaultPort(scheme)
	}
	if port == "" {
		return false
	}
	target := net.JoinHostPort(host, port)

	// IP-литерал допускаем только если он явно задан оператором и не private.
	if ip := net.ParseIP(host); ip != nil {
		return operatorTargets()[target] && !isBlockedIP(ip)
	}
	// Доверяем только точной паре host:port из конфига оператора.
	if operatorTargets()[target] {
		return true
	}
	if (port != "80" && port != "443") || !domainAllowed(host) {
		return false
	}
	// Публичный домен из allowlist: дополнительно смотрим, куда он резолвится.
	return hostResolvesPublic(host)
}

// ============================================================
// HTTP-КЛИЕНТ
// ============================================================
var (
	clientOnce   sync.Once
	sharedClient *http.Client
)

func makeClient() *http.Client {
	clientOnce.Do(func() {
		sharedClient = buildClient()
	})
	return sharedClient
}

func buildClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:            pinnedDialContext(dialer),
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		IdleConnTimeout:        30 * time.Second,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    8,
		MaxResponseHeaderBytes: 1 << 20,
		ForceAttemptHTTP2:      true,
	}

	// Прокси читается в момент сборки клиента, а не из мутируемого глобала.
	if proxy := platform.GetEnv("HTTP_PROXY", ""); proxy != "" {
		if proxyURL, err := url.Parse(proxy); err == nil && proxyURL.Host != "" {
			transport.Proxy = proxyURLFunc(proxyURL)
			// Резолв и проверку адреса делает прокси.
			transport.DialContext = nil
		} else {
			log.Printf("⚠️ HTTP_PROXY не разобран — egress пойдёт напрямую")
		}
	}

	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		// Редирект не должен уводить запрос во внутреннюю сеть.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errTooManyRedirects
			}
			if !isSafeURL(req.URL.String()) {
				return errRedirectBlocked
			}
			return nil
		},
	}
}

// proxyURLFunc проксирует всё, кроме loopback: локальные адреса не должны
// уходить во внешний прокси (и тесты не зависят от окружения раннера).
func proxyURLFunc(proxyURL *url.URL) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if ip := net.ParseIP(req.URL.Hostname()); ip != nil && ip.IsLoopback() {
			return nil, nil
		}
		return proxyURL, nil
	}
}

// pinnedDialContext закрывает DNS-rebinding: соединение идёт на уже
// проверенный IP, а не на результат повторного резолва в Transport.
func pinnedDialContext(d *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))

		if operatorTargets()[net.JoinHostPort(host, port)] {
			return d.DialContext(ctx, network, addr)
		}
		if ip := net.ParseIP(host); ip != nil {
			if isBlockedIP(ip) {
				return nil, fmt.Errorf("books-service: blocked address %q", host)
			}
			return d.DialContext(ctx, network, addr)
		}

		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		ips, err := lookupIPAddr(lookupCtx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("books-service: lookup %q failed", host)
		}

		var lastErr error
		for _, ip := range ips {
			if isBlockedIP(ip.IP) {
				lastErr = fmt.Errorf("books-service: %q resolves to blocked address", host)
				continue
			}
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("books-service: no address for %q", host)
		}
		return nil, lastErr
	}
}

func newRequest(method, u string) (*http.Request, error) {
	return newRequestCtx(context.Background(), method, u)
}

func newRequestCtx(ctx context.Context, method, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) books-service/1.0")
	req.Header.Set("Accept", "application/atom+xml,application/xml;q=0.9,*/*;q=0.8")
	return req, nil
}

// ============================================================
// ПОИСК КНИГ
// ============================================================
// searchPath — формат поиска зависит от источника: локальное zlsl-зеркало
// понимает /opds/search?q=, flibusta-зеркала — searchType/searchTerm,
// прочие OPDS — query.
func searchPath(base, query string) string {
	q := url.QueryEscape(query)
	b := strings.TrimRight(strings.TrimSpace(base), "/")

	if local := strings.TrimRight(strings.TrimSpace(platform.GetEnv("FLIBUSTA_LOCAL_URL", "")), "/"); local != "" && strings.EqualFold(b, local) {
		return "/opds/search?q=" + q
	}
	// Формат выбирается по allowlist-домену, а не по подстроке в URL:
	// https://evil-flibusta.example не должен получать flibusta-формат.
	if domainAllowed(hostOf(b)) {
		return "/opds/search?searchType=books&searchTerm=" + q
	}
	return "/ebooks/search.opds/?query=" + q
}

func searchBooks(query string) ([]fiber.Map, error) {
	// Путь зависит от зеркала (/opds/search?q= vs searchType/searchTerm),
	// поэтому строим его внутри fallback-цикла, а не один раз.
	resp, usedBase, err := fetchWithFallbackFunc(func(base string) string {
		return searchPath(base, query)
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("source returned %d", resp.StatusCode)
	}

	body, err := readLimited(resp.Body, maxFeedBytes)
	if err != nil {
		return nil, err
	}

	var feed Feed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("opds parse error: %w", err)
	}

	baseURL, err := url.Parse(strings.TrimRight(usedBase, "/") + "/")
	if err != nil {
		return nil, err
	}

	books := []fiber.Map{}
	for _, entry := range feed.Entries {
		if entry.ID == "tag:search:author" || entry.ID == "tag:search:books" {
			continue
		}

		title := plainText(entry.Title, 300)
		if title == "" {
			continue
		}

		book := fiber.Map{
			"title": title,
			"id":    plainText(entry.ID, 200),
		}

		if len(entry.Authors) > 0 {
			if author := plainText(entry.Authors[0].Name, 200); author != "" {
				book["author"] = author
			}
		}

		// Поля из OPDS — недоверенные: year/language уходят в innerHTML дашборда.
		if y := strings.TrimSpace(entry.Issued); reYear.MatchString(y) {
			book["year"] = y
		}
		if l := strings.TrimSpace(entry.Language); reLang.MatchString(l) {
			book["language"] = strings.ToLower(l)
		}
		if f := plainText(entry.Format, 32); f != "" {
			book["format"] = f
		}
		// description отдаём как plain text, без HTML.
		if d := plainText(entry.Content, 2000); d != "" {
			book["description"] = d
		}

		formats := fiber.Map{}
		var coverURL, fallbackID string

		for _, link := range entry.Links {
			rel := strings.TrimSpace(link.Rel)
			href := strings.TrimSpace(link.Href)
			typ := strings.TrimSpace(link.Type)
			if href == "" || strings.ContainsAny(href, "\r\n") {
				continue
			}

			ref, err := url.Parse(href)
			if err != nil || ref.User != nil {
				continue
			}
			fullURL := baseURL.ResolveReference(ref).String()
			if !strings.HasPrefix(fullURL, "http://") && !strings.HasPrefix(fullURL, "https://") {
				continue
			}

			if strings.Contains(rel, "acquisition") {
				switch {
				case strings.Contains(typ, "epub"):
					formats["epub"] = fullURL
				case strings.Contains(typ, "mobipocket") || strings.Contains(typ, "mobi"):
					formats["mobi"] = fullURL
				case strings.Contains(typ, "fb2"):
					formats["fb2"] = fullURL
				case strings.Contains(typ, "text/plain"):
					formats["txt"] = fullURL
				}
			}

			if rel == "http://opds-spec.org/image" {
				coverURL = fullURL
			}
			if rel == "http://opds-spec.org/image/thumbnail" && coverURL == "" {
				coverURL = fullURL
			}

			if strings.Contains(href, "/b/") {
				if id := extractBookID(href); id != "" {
					fallbackID = id
				}
			}
		}

		if len(formats) == 0 && isNumericID(fallbackID) {
			formats = fiber.Map{
				"fb2":  strings.TrimRight(usedBase, "/") + "/b/" + fallbackID + "/fb2",
				"epub": strings.TrimRight(usedBase, "/") + "/b/" + fallbackID + "/epub",
				"mobi": strings.TrimRight(usedBase, "/") + "/b/" + fallbackID + "/mobi",
			}
		}

		if len(formats) == 0 && !strings.Contains(usedBase, "flibusta") {
			if bookID := extractBookID(entry.ID); bookID != "" {
				formats = fiber.Map{
					"epub": fmt.Sprintf("%s/ebooks/%s.epub.noimages", strings.TrimRight(usedBase, "/"), bookID),
					"mobi": fmt.Sprintf("%s/ebooks/%s.kindle.noimages", strings.TrimRight(usedBase, "/"), bookID),
					"txt":  fmt.Sprintf("%s/ebooks/%s.txt.utf-8", strings.TrimRight(usedBase, "/"), bookID),
				}
				book["id"] = bookID
			}
		}

		book["formats"] = formats
		if coverURL != "" {
			book["cover"] = coverURL
		}
		books = append(books, book)
	}

	return books, nil
}

// plainText убирает HTML-теги и управляющие символы: OPDS-поля недоверенные
// и попадают в innerHTML дашборда.
func plainText(s string, maxRunes int) string {
	s = html.UnescapeString(reTags.ReplaceAllString(s, " "))
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes > 0 && utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return s
}

// ============================================================
// СКАЧИВАНИЕ
// ============================================================
// errBadRequest — ошибка валидации входных данных /download.
// Отличается от сетевых ошибок, чтобы хендлер отдавал 400, а не 500.
var errBadRequest = errors.New("bad request")

// allowedDownloadFormats — whitelist форматов: значение попадает в имя файла,
// поэтому всё, кроме коротких буквенных токенов, запрещено.
var allowedDownloadFormats = map[string]bool{
	"epub": true,
	"fb2":  true,
	"mobi": true,
	"txt":  true,
}

// formatExt — путь файла на зеркале для каждого формата.
var formatExt = map[string]string{
	"epub": "epub.noimages",
	"mobi": "kindle.noimages",
	"txt":  "txt.utf-8",
}

// normalizeFormat — канонический вид формата + проверка whitelist.
func normalizeFormat(format string) (string, error) {
	f := strings.ToLower(strings.TrimSpace(format))
	if !allowedDownloadFormats[f] {
		return "", fmt.Errorf("%w: invalid format: %q", errBadRequest, format)
	}
	return f, nil
}

// hostOf — хост (без порта) из URL.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

// trustedDownloadHosts — whitelist хостов для скачивания: только настроенные
// зеркала Флибусты и локальное зеркало. Всё остальное (169.254.169.254,
// localhost, 10.x, внутренние хосты, чужие домены) отсекается — защита от SSRF.
func trustedDownloadHosts() map[string]bool {
	out := map[string]bool{}
	add := func(raw string) {
		if h := hostOf(raw); h != "" {
			out[h] = true
		}
	}
	for _, m := range getFlibustaMirrors() {
		add(m)
	}
	add(platform.GetEnv("FLIBUSTA_LOCAL_URL", ""))
	add(sourceURL)
	return out
}

// isNumericID — строго цифровой идентификатор книги.
func isNumericID(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolveDownloadURL — строит URL строго из доверенных зеркал.
// Полный URL от клиента допускается только если его хост в whitelist.
func resolveDownloadURL(id, format string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("%w: id is required", errBadRequest)
	}
	lower := strings.ToLower(id)

	switch {
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		u, err := url.Parse(id)
		if err != nil || u.User != nil {
			return "", fmt.Errorf("%w: invalid url", errBadRequest)
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		port := u.Port()
		if port == "" {
			port = defaultPort(u.Scheme)
		}
		if host == "" || port == "" || !operatorTargets()[net.JoinHostPort(host, port)] {
			return "", fmt.Errorf("%w: host is not an allowed mirror: %q", errBadRequest, host)
		}
		return id, nil

	case strings.Contains(id, "://"):
		// file://, ftp://, gopher:// и прочее не поддерживаем.
		return "", fmt.Errorf("%w: unsupported url scheme", errBadRequest)

	case strings.HasPrefix(id, "/"):
		// Относительный путь зеркала (/b/<id>/fb2).
		if strings.Contains(id, "..") || strings.ContainsAny(id, "\\\x00") {
			return "", fmt.Errorf("%w: invalid id path", errBadRequest)
		}
		return flibustaBase() + id, nil

	default:
		bookID := extractBookID(id)
		if bookID == "" {
			bookID = id
		}
		if !isNumericID(bookID) {
			return "", fmt.Errorf("%w: invalid id: %q", errBadRequest, id)
		}
		ext, ok := formatExt[format]
		if !ok {
			return "", fmt.Errorf("%w: format %q requires a direct url", errBadRequest, format)
		}
		return fmt.Sprintf("%s/ebooks/%s.%s", flibustaBase(), bookID, ext), nil
	}
}

// safeBookPath — итоговый путь файла книги: гарантирует, что файл окажется
// строго внутри booksDir (защита от path traversal через title/format).
func safeBookPath(title, format string) (string, error) {
	name := sanitizeFilename(title)
	if name == "" {
		name = "book"
	}
	name = name + "." + format

	// Даже после sanitizeFilename имя не должно содержать разделителей пути.
	if strings.ContainsAny(name, "/\\") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: invalid filename", errBadRequest)
	}

	absDir, err := filepath.Abs(booksDir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errBadRequest, err)
	}
	dest := filepath.Join(booksDir, name)
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errBadRequest, err)
	}
	if absDest == absDir || !strings.HasPrefix(absDest, absDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: path traversal detected", errBadRequest)
	}
	return dest, nil
}

func downloadBook(id, title, format string) error {
	return downloadBookContext(context.Background(), id, title, format)
}

func downloadBookContext(ctx context.Context, id, title, format string) error {
	// 1) Формат — только из whitelist: значение попадает в имя файла.
	format, err := normalizeFormat(format)
	if err != nil {
		return err
	}

	// 2) URL — только с доверенных зеркал (защита от SSRF).
	downloadURL, err := resolveDownloadURL(id, format)
	if err != nil {
		return err
	}

	// 3) Путь файла — строго внутри booksDir (защита от path traversal).
	destPath, err := safeBookPath(title, format)
	if err != nil {
		return err
	}

	// Не перезаписываем существующие книги (в т.ч. чужие симлинки в шаре):
	// os.Create пошёл бы по ссылке и записал за пределы booksDir.
	if _, err := os.Lstat(destPath); err == nil {
		return fmt.Errorf("%w: %s", errConflict, filepath.Base(destPath))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot stat destination: %w", err)
	}

	req, err := newRequestCtx(ctx, http.MethodGet, downloadURL)
	if err != nil {
		return err
	}

	resp, err := makeClient().Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBookBytes {
		return errTooLarge
	}

	tmp, err := os.CreateTemp(booksDir, ".incoming-*")
	if err != nil {
		return fmt.Errorf("cannot create file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxBookBytes+1))
	if err != nil {
		cleanup()
		return fmt.Errorf("cannot write file: %w", err)
	}
	if n > maxBookBytes {
		cleanup()
		return errTooLarge
	}
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}

	// Если формат fb2 и файл — zip, распаковываем во временный файл.
	if format == "fb2" {
		if err := unpackFB2(tmpName); err != nil {
			log.Printf("⚠️ Не удалось распаковать FB2: %v", err)
		}
	}

	// Rename атомарен и заменяет directory entry, а не цель симлинка.
	if err := os.Rename(tmpName, destPath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("cannot publish file: %w", err)
	}

	log.Printf("📚 Скачано: %s (%d bytes)", destPath, n)
	return nil
}

// ============================================================
// РАСПАКОВКА FB2
// ============================================================
func unpackFB2(src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Проверяем, zip ли это
	header := make([]byte, 4)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil // слишком короткий файл — не zip
	}
	if string(header) != "PK\x03\x04" {
		return nil // не zip
	}

	// Получаем размер
	info, err := f.Stat()
	if err != nil {
		return err
	}

	// Открываем как zip
	zipReader, err := zip.NewReader(f, info.Size())
	if err != nil {
		return err
	}

	// Ищем первый .fb2 в архиве
	for _, zf := range zipReader.File {
		if !strings.HasSuffix(strings.ToLower(zf.Name), ".fb2") {
			continue
		}
		if zf.UncompressedSize64 > uint64(maxFB2Bytes) {
			return errTooLarge
		}

		rc, err := zf.Open()
		if err != nil {
			return err
		}
		data, err := readLimited(rc, maxFB2Bytes)
		_ = rc.Close()
		if err != nil {
			return err
		}

		if err := os.WriteFile(src, data, 0o644); err != nil {
			return err
		}

		log.Printf("📖 Распакован FB2: %s (%d bytes)", src, len(data))
		return nil
	}

	return fmt.Errorf("fb2 не найден в архиве")
}

// ============================================================
// УТИЛИТЫ
// ============================================================
// extractBookID — строгий парсер: только цифры, никаких "12abc"/"+12".
func extractBookID(input string) string {
	s := strings.TrimSpace(input)
	if isNumericID(s) {
		return s
	}

	if u, err := url.Parse(s); err == nil && u.Path != "" {
		segments := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i := 0; i+1 < len(segments); i++ {
			if segments[i] == "b" && isNumericID(segments[i+1]) {
				return segments[i+1]
			}
		}
		if n := len(segments); n > 0 && isNumericID(strings.TrimSuffix(segments[n-1], ".opds")) {
			return strings.TrimSuffix(segments[n-1], ".opds")
		}
	}

	parts := strings.Split(strings.Trim(s, "/"), "/")
	if n := len(parts); n > 0 {
		last := strings.TrimSuffix(parts[n-1], ".opds")
		if isNumericID(last) {
			return last
		}
		// OPDS-идентификаторы вида "tag:book:6013".
		if i := strings.LastIndexByte(last, ':'); i >= 0 && isNumericID(last[i+1:]) {
			return last[i+1:]
		}
	}
	return ""
}

// trailingDigits — числовой id из "6013", "tag:book:6013" или URL.
func trailingDigits(s string) string {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	return s[i:]
}

// readURL — URL для скачивания fb2 с учётом типа зеркала.
func readURL(base, id string) string {
	id = strings.TrimSpace(id)
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		// Defense in depth: даже если вызывающий забудет isSafeURL,
		// абсолютный URL не станет SSRF-sink'ом.
		if !isSafeURL(id) {
			return ""
		}
		return id
	}
	base = strings.TrimRight(base, "/")
	if strings.HasPrefix(id, "/") {
		return base + id
	}
	num := trailingDigits(id)
	if num == "" {
		return ""
	}
	if local := strings.TrimRight(platform.GetEnv("FLIBUSTA_LOCAL_URL", ""), "/"); local != "" && base == local {
		return local + "/fb2.php?id=" + num
	}
	return base + "/b/" + num + "/fb2"
}

func sanitizeFilename(name string) string {
	name = strings.ToValidUTF8(name, "")
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return -1
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			return '_'
		}
		return r
	}, name)

	name = strings.Trim(name, " .")
	if runes := []rune(name); len(runes) > 180 {
		name = string(runes[:180])
	}
	return name
}

// storedBookExts — что вообще разрешено отдавать из booksDir.
var storedBookExts = map[string]bool{
	".epub": true, ".fb2": true, ".pdf": true, ".mobi": true,
	".djvu": true, ".cbz": true, ".azw3": true, ".txt": true,
}

// resolveStoredBook — имя без сегментов пути, после EvalSymlinks внутри booksDir.
func resolveStoredBook(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." ||
		name != filepath.Base(name) || strings.HasPrefix(name, ".") ||
		strings.ContainsRune(name, 0) || utf8.RuneCountInString(name) > 255 {
		return "", errBadRequest
	}
	if !storedBookExts[strings.ToLower(filepath.Ext(name))] {
		return "", errBadRequest
	}

	realRoot, err := filepath.EvalSymlinks(booksDir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(filepath.Join(booksDir, name))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errBadRequest
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errBadRequest
	}
	return real, nil
}

func sendBookFile(c fiber.Ctx, path string) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".epub":
		c.Set("Content-Type", "application/epub+zip")
	case ".fb2":
		c.Set("Content-Type", "application/x-fictionbook+xml; charset=utf-8")
	case ".pdf":
		c.Set("Content-Type", "application/pdf")
	default:
		c.Set("Content-Type", "application/octet-stream")
	}
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", asciiFilename(filepath.Base(path))))
	return c.SendFile(path)
}

func safeID(id string) string {
	if n := extractBookID(id); n != "" {
		return n
	}
	return "book"
}

func asciiFilename(s string) string {
	s = filepath.Base(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "book"
	}
	return b.String()
}
