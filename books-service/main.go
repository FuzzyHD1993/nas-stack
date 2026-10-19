package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ============================================================
// КОНФИГ
// ============================================================
var (
	sourceURL = platform.GetEnv("FLIBUSTA_URL", "https://flibusta.is")
	httpProxy = platform.GetEnv("HTTP_PROXY", "")
	booksDir  = platform.GetEnv("BOOKS_DIR", "/books")
)

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
func main() {
	if err := os.MkdirAll(booksDir, 0o755); err != nil {
		log.Printf("⚠️ Cannot create books dir: %v", err)
	}

	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error {
		return c.JSON(platform.HealthJSON("books", fiber.Map{"source": flibustaBase()}))
	})

	app.Get("/search", func(c fiber.Ctx) error {
		query := c.Query("q")
		if query == "" {
			return c.Status(400).JSON(fiber.Map{"error": "query is required"})
		}

		books, err := searchBooks(query)
		if err != nil {
			return c.Status(503).JSON(fiber.Map{
				"error":         "Флибуста временно недоступна. Все зеркала не отвечают.",
				"hint":          "Можно развернуть локальное зеркало (профиль flibusta-local) и не зависеть от серверов Флибусты.",
				"docs_url":      "https://github.com/FuzzyHD1993/nas-stack/blob/main/docs/local-flibusta.md",
				"mirrors_tried": getFlibustaSources(),
				"detail":        err.Error(),
			})
		}

		return c.JSON(fiber.Map{
			"query": query,
			"count": len(books),
			"books": books,
		})
	})

	// GET /read/:id — прокси книги с локального зеркала (или онлайн).
	// Отдаёт .fb2 как файл: браузер не умеет открывать FB2 (это XML).
	app.Get("/read/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		// Сначала — локально скачанный файл (EPUB-читалка и скачанные FB2).
		if name, err := url.QueryUnescape(id); err == nil && name != "" &&
			!strings.Contains(name, "..") && !strings.ContainsAny(name, "/\\") {
			path := filepath.Join(booksDir, name)
			if _, statErr := os.Stat(path); statErr == nil {
				switch strings.ToLower(filepath.Ext(name)) {
				case ".epub":
					c.Set("Content-Type", "application/epub+zip")
				case ".fb2":
					c.Set("Content-Type", "application/x-fictionbook+xml")
				default:
					c.Set("Content-Type", "application/octet-stream")
				}
				c.Set("Accept-Ranges", "bytes")
				return c.SendFile(path)
			}
		}
		hc := makeClient()
		notFound := false
		for _, base := range getFlibustaSources() {
			u := readURL(base, id)
			if u == "" {
				continue
			}
			if !isSafeURL(u) {
				// Клиент мог прислать свой URL (readURL возвращает его как есть) —
				// во внутреннюю сеть не ходим.
				return c.Status(400).JSON(fiber.Map{"error": "unsafe url: " + u})
			}
			req, err := newRequest("GET", u)
			if err != nil {
				continue
			}
			resp, err := hc.Do(req)
			if err != nil {
				continue
			}
			if resp.StatusCode == 200 {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				trimmed := bytes.TrimSpace(body)
				if !bytes.HasPrefix(trimmed, []byte("<?xml")) && !bytes.HasPrefix(trimmed, []byte("<FictionBook")) {
					notFound = true
					break // зеркало вернуло "NO ZIP" или заглушку
				}
				num := trailingDigits(id)
				if num == "" {
					num = "book"
				}
				c.Set("Content-Type", "application/fb2+xml; charset=utf-8")
				return c.Send(body)
			}
			_ = resp.Body.Close()
		}
		if notFound {
			return c.Status(404).JSON(fiber.Map{"error": "book text not found (not in the local archive)"})
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

		if err := downloadBook(body.ID, body.Title, body.Format); err != nil {
			// Ошибки валидации входа (format / id / path) — это 400, а не 500.
			if errors.Is(err, errBadRequest) {
				return c.Status(400).JSON(fiber.Map{"error": err.Error()})
			}
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		return c.JSON(fiber.Map{"status": "downloaded", "title": body.Title})
	})

	app.Get("/downloaded", func(c fiber.Ctx) error {
		files, err := os.ReadDir(booksDir)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		books := []fiber.Map{}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			info, _ := f.Info()
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
		default:
			contentType = "image/jpeg"
		}
		c.Set("Content-Type", contentType)
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Cache-Control", "public, max-age=86400")

		body, _ := io.ReadAll(resp.Body)
		return c.Send(body)
	})

	log.Printf("📚 books-service на :8087 (source: %s)", flibustaBase())
	log.Fatal(app.Listen(":8087"))
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
// unspecified и multicast.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 — CGNAT (часто используется для внутренних сервисов).
		if v4[0] == 100 && v4[1]&0xc0 == 64 {
			return true
		}
		return false
	}
	// fc00::/7 — Unique Local Address.
	return len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc
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

// domainAllowed — домен из allowlist Флибусты (или его поддомен).
func domainAllowed(host string) bool {
	for _, d := range allowedFetchDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// DNS-кэш: резолвим каждый хост не чаще раза в 5 минут.
type dnsVerdict struct {
	public bool
	at     time.Time
}

var (
	dnsMu    sync.Mutex
	dnsCache = map[string]dnsVerdict{}
)

// hostResolvesPublic — резолвит домен и проверяет, что ВСЕ адреса публичные.
// Любой private/loopback/link-local адрес в ответе DNS → отказ (защита и от
// подмены DNS, и от DNS-rebinding).
func hostResolvesPublic(host string) bool {
	const ttl = 5 * time.Minute

	dnsMu.Lock()
	if v, ok := dnsCache[host]; ok && time.Since(v.at) < ttl {
		dnsMu.Unlock()
		return v.public
	}
	dnsMu.Unlock()

	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	public := true
	for _, ip := range ips {
		if isBlockedIP(ip) {
			public = false
			break
		}
	}

	dnsMu.Lock()
	dnsCache[host] = dnsVerdict{public: public, at: time.Now()}
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
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		// file://, gopher://, ftp:// и прочие схемы не поддерживаем.
		return false
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || blockedHostNames[host] || strings.HasSuffix(host, ".localhost") {
		return false
	}

	// IP-литерал допускаем только если он явно задан оператором и не private.
	if ip := net.ParseIP(host); ip != nil {
		return operatorHosts()[host] && !isBlockedIP(ip)
	}

	if operatorHosts()[host] {
		return true
	}
	if !domainAllowed(host) {
		return false
	}
	// Публичный домен из allowlist: дополнительно смотрим, куда он резолвится.
	return hostResolvesPublic(host)
}

// ============================================================
// HTTP-КЛИЕНТ
// ============================================================
func makeClient() *http.Client {
	transport := &http.Transport{}

	if httpProxy != "" {
		if proxyURL, err := url.Parse(httpProxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		// Редирект не должен уводить запрос во внутреннюю сеть.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !isSafeURL(req.URL.String()) {
				return errors.New("books-service: redirect to disallowed host")
			}
			return nil
		},
	}
}

func newRequest(method, u string) (*http.Request, error) {
	req, err := http.NewRequest(method, u, nil)
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
// понимает /opds/search?q=, flibusta.is — searchType/searchTerm, прочие OPDS — query.
func searchPath(base, query string) string {
	q := url.QueryEscape(query)
	if local := strings.TrimRight(platform.GetEnv("FLIBUSTA_LOCAL_URL", ""), "/"); local != "" && base == local {
		return "/opds/search?q=" + q
	}
	if strings.Contains(base, "flibusta") {
		return "/opds/search?searchType=books&searchTerm=" + q
	}
	return "/ebooks/search.opds/?query=" + q
}

func searchBooks(query string) ([]fiber.Map, error) {
	base := flibustaBase()
	path := searchPath(base, query)

	resp, err := fetchWithFallback(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("source returned %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)

	var feed Feed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("opds parse error: %w", err)
	}

	books := []fiber.Map{}
	for _, entry := range feed.Entries {
		if entry.ID == "tag:search:author" || entry.ID == "tag:search:books" {
			continue
		}

		book := fiber.Map{
			"title": entry.Title,
			"id":    entry.ID,
		}

		for _, a := range entry.Authors {
			book["author"] = a.Name
			break
		}

		if entry.Issued != "" {
			book["year"] = entry.Issued
		}
		if entry.Language != "" {
			book["language"] = entry.Language
		}
		if entry.Format != "" {
			book["format"] = entry.Format
		}
		if entry.Content != "" {
			book["description"] = entry.Content
		}

		formats := fiber.Map{}
		var coverURL string
		var fallbackID string

		for _, link := range entry.Links {
			rel := strings.TrimSpace(link.Rel)
			href := strings.TrimSpace(link.Href)
			typ := strings.TrimSpace(link.Type)

			fullURL := href
			if strings.HasPrefix(href, "/") {
				fullURL = flibustaBase() + href
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
				parts := strings.Split(href, "/")
				for i, p := range parts {
					if p == "b" && i+1 < len(parts) {
						fallbackID = parts[i+1]
						break
					}
				}
			}
		}

		if len(formats) == 0 && fallbackID != "" {
			formats = fiber.Map{
				"fb2":  flibustaBase() + "/b/" + fallbackID + "/fb2",
				"epub": flibustaBase() + "/b/" + fallbackID + "/epub",
				"mobi": flibustaBase() + "/b/" + fallbackID + "/mobi",
			}
		}

		if len(formats) == 0 && !strings.Contains(flibustaBase(), "flibusta") {
			bookID := extractBookID(entry.ID)
			if bookID != "" {
				formats = fiber.Map{
					"epub": fmt.Sprintf("%s/ebooks/%s.epub.noimages", flibustaBase(), bookID),
					"mobi": fmt.Sprintf("%s/ebooks/%s.kindle.noimages", flibustaBase(), bookID),
					"txt":  fmt.Sprintf("%s/ebooks/%s.txt.utf-8", flibustaBase(), bookID),
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
	return strings.ToLower(u.Hostname())
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
		if err != nil {
			return "", fmt.Errorf("%w: invalid url", errBadRequest)
		}
		host := strings.ToLower(u.Hostname())
		if host == "" || !trustedDownloadHosts()[host] {
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

	client := makeClient()
	req, err := newRequest("GET", downloadURL)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("cannot create file: %w", err)
	}

	written, err := io.Copy(out, resp.Body)
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("cannot write file: %w", err)
	}
	_ = out.Close()

	log.Printf("📚 Скачано: %s (%d bytes)", destPath, written)

	// Если формат fb2 и файл — zip, распаковываем
	if format == "fb2" {
		if err := unpackFB2(destPath); err != nil {
			log.Printf("⚠️ Не удалось распаковать FB2: %v", err)
		}
	}

	return nil
}

// ============================================================
// РАСПАКОВКА FB2
// ============================================================
func unpackFB2(filepath string) error {
	f, err := os.Open(filepath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Проверяем, zip ли это
	header := make([]byte, 4)
	if _, err := f.Read(header); err != nil {
		return err
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
		if strings.HasSuffix(strings.ToLower(zf.Name), ".fb2") {
			rc, err := zf.Open()
			if err != nil {
				return err
			}

			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return err
			}

			if err := os.WriteFile(filepath, data, 0o644); err != nil {
				return err
			}

			log.Printf("📖 Распакован FB2: %s (%d bytes)", filepath, len(data))
			return nil
		}
	}

	return fmt.Errorf("fb2 не найден в архиве")
}

// ============================================================
// УТИЛИТЫ
// ============================================================
func extractBookID(input string) string {
	s := input
	s = strings.TrimSuffix(s, ".opds")
	s = strings.TrimSuffix(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	if _, err := fmt.Sscanf(last, "%d", new(int)); err != nil {
		return ""
	}
	return last
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
	if strings.HasPrefix(id, "http") {
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
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
	)
	result := replacer.Replace(name)
	result = strings.TrimSpace(result)
	if len(result) > 200 {
		result = result[:200]
	}
	return result
}
