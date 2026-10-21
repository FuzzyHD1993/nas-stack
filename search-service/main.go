package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const (
	defaultLimit = 100
	maxLimit     = 100
	maxOffset    = 10_000
	maxQueryLen  = 256
	maxParamLen  = 256
	maxBodyBytes = 16 << 20
	maxURLLen    = 4096

	// Файл .torrent небольшой; больше этого не отдаём и не буферизуем.
	maxDownloadBytes = 16 << 20

	prowlarrTimeout    = 45 * time.Second
	cacheTTL           = time.Hour
	cacheTTLJitterSecs = 300
)

var (
	prowlarrURL  = strings.TrimRight(platform.GetEnv("PROWLARR_URL", "http://host.containers.internal:9696"), "/")
	prowlarrHost = hostOf(prowlarrURL)
	apiKey       = strings.TrimSpace(platform.GetEnv("PROWLARR_API_KEY", ""))
	rdb          *redis.Client

	// downloadBaseURL — внешний адрес прокси скачивания (шлюз). Клиент
	// получает ссылку без apikey; ключ добавляет сам сервис.
	downloadBaseURL = strings.TrimRight(platform.GetEnv("DOWNLOAD_BASE_URL", "http://host.containers.internal:8080"), "/")

	// Только путь скачивания индексатора Prowlarr: произвольные GET к API
	// через прокси невозможны.
	downloadPathRe = regexp.MustCompile(`^/(?:api/v1/indexer/)?[0-9]{1,12}/download$`)

	errUpstream = errors.New("prowlarr unavailable")

	// Один клиент: keep-alive и запрет редиректов, чтобы X-Api-Key не ушёл
	// на чужой хост через 302.
	prowlarrHTTP = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	indexersCache = struct {
		mu   sync.Mutex
		at   time.Time
		data []fiber.Map
	}{}

	// Схлопывание одновременных промахов кэша: N одинаковых запросов —
	// 1 поход в Prowlarr.
	searchGroup   singleflight.Group
	indexersGroup singleflight.Group
)

func main() {
	if err := platform.ValidateEnv([]string{"PROWLARR_API_KEY"}); err != nil {
		log.Fatalf("❌ %v", err)
	}
	if prowlarrHost == "" {
		log.Fatalf("❌ PROWLARR_URL=%q: нужен http(s) URL с хостом", prowlarrURL)
	}
	if u, err := url.Parse(prowlarrURL); err != nil || u.User != nil {
		log.Fatalf("❌ PROWLARR_URL=%q: userinfo в URL запрещён", prowlarrURL)
	}

	rdb = redis.NewClient(&redis.Options{
		Addr:         platform.GetEnv("REDIS_ADDR", "redis:6379"),
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     20,
	})
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 2*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		// Redis — только кэш: старт не блокируем, но и не молчим.
		log.Printf("⚠️ redis: %v (кэш поиска недоступен)", err)
	}
	cancelPing()

	app := newFiber()
	platform.StartPprof()
	registerRoutes(app)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		if err := app.ShutdownWithTimeout(10 * time.Second); err != nil {
			log.Printf("shutdown: %v", err)
		}
		_ = rdb.Close()
	}()

	log.Println("🔍 search-service на :8088")
	if err := app.Listen(":8088"); err != nil {
		log.Printf("listen: %v", err)
	}
}

func newFiber() *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:    1 << 20,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 70 * time.Second,
		IdleTimeout:  60 * time.Second,
	})
	// metrics ДО recover: паника должна попасть в метрики как 500.
	app.Use(metricsMiddleware())
	app.Use(recover.New())
	return app
}

func registerRoutes(app *fiber.App) {
	app.Get("/metrics", adaptor.HTTPHandler(metricsHandler()))
	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("search", nil)) })
	app.Get("/search", searchHandler)
	// /download — путь за gateway (/search/download → /download); второй
	// маршрут — если DOWNLOAD_BASE_URL указывает прямо на сервис.
	app.Get("/download", downloadHandler)
	app.Get("/search/download", downloadHandler)
	app.Get("/indexers", indexersHandler)
}

func searchHandler(c fiber.Ctx) error {
	query := strings.TrimSpace(c.Query("q"))
	if query == "" || len([]rune(query)) > maxQueryLen {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid query"})
	}

	categories, ok := parseIDList(c.Query("categories"), maxParamLen)
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid categories"})
	}
	indexerIDs, ok := parseIDList(c.Query("indexerIds"), maxParamLen)
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid indexerIds"})
	}

	limit, err := parseBounded(c.Query("limit"), defaultLimit, 1, maxLimit)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid limit"})
	}
	offset, err := parseBounded(c.Query("offset"), 0, 0, maxOffset)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid offset"})
	}

	reqCtx, cancel := context.WithTimeout(c.Context(), 65*time.Second)
	defer cancel()

	results, err := searchTorrents(reqCtx, query, categories, indexerIDs, limit, offset)
	if err != nil {
		// Детали апстрима/сети — только в лог, клиенту — общий ответ.
		log.Printf("search: %v", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "upstream unavailable"})
	}

	return c.JSON(fiber.Map{
		"query":   query,
		"count":   len(results),
		"limit":   limit,
		"offset":  offset,
		"results": results,
	})
}

func indexersHandler(c fiber.Ctx) error {
	if data, ok := indexersCached(); ok {
		return c.JSON(fiber.Map{"indexers": data})
	}

	reqCtx, cancel := context.WithTimeout(c.Context(), 40*time.Second)
	defer cancel()

	v, err, _ := indexersGroup.Do("indexers", func() (any, error) {
		if data, ok := indexersCached(); ok {
			return data, nil
		}
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(reqCtx), prowlarrTimeout)
		defer fcancel()

		indexers, err := listIndexers(fctx)
		if err != nil {
			return nil, err
		}
		indexersCache.mu.Lock()
		indexersCache.at = time.Now()
		indexersCache.data = indexers
		indexersCache.mu.Unlock()
		return indexers, nil
	})
	if err != nil {
		log.Printf("indexers: %v", err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "upstream unavailable"})
	}

	indexers, _ := v.([]fiber.Map)
	return c.JSON(fiber.Map{"indexers": indexers})
}

func indexersCached() ([]fiber.Map, bool) {
	indexersCache.mu.Lock()
	defer indexersCache.mu.Unlock()
	if time.Since(indexersCache.at) < 5*time.Minute && indexersCache.data != nil {
		return indexersCache.data, true
	}
	return nil, false
}

func searchTorrents(rctx context.Context, query, categories, indexerIDs string, limit, offset int) ([]fiber.Map, error) {
	key := makeCacheKey(query, categories, indexerIDs, limit, offset)

	if results, ok := cacheGet(rctx, key); ok {
		return results, nil
	}

	ch := searchGroup.DoChan(key, func() (any, error) {
		// Лидер не должен отменяться обрывом своего клиента: результат
		// нужен всем ожидающим.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(rctx), prowlarrTimeout)
		defer cancel()

		// Пока ждали singleflight, лидер мог уже заполнить кэш.
		if results, ok := cacheGet(fctx, key); ok {
			return results, nil
		}
		return fetchTorrents(fctx, key, query, categories, indexerIDs, limit, offset)
	})

	select {
	case <-rctx.Done():
		return nil, rctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		results, ok := res.Val.([]fiber.Map)
		if !ok {
			return nil, errUpstream
		}
		return results, nil
	}
}

func fetchTorrents(rctx context.Context, key, query, categories, indexerIDs string, limit, offset int) ([]fiber.Map, error) {
	// Сам запрос в лог не пишем: это история поиска пользователей.
	log.Printf("🔍 Prowlarr: q#%s (len=%d, categories=%q, indexerIds=%q, limit=%d, offset=%d)",
		hashQuery(query), len([]rune(query)),
		truncateLog(categories, 128), truncateLog(indexerIDs, 128), limit, offset)

	params := url.Values{}
	params.Set("query", query)
	params.Set("type", "search")
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", strconv.Itoa(offset))
	if categories != "" {
		params.Set("categories", categories)
	}
	if indexerIDs != "" {
		params.Set("indexerIds", indexerIDs)
	}

	var raw []map[string]any
	if err := prowlarrGetJSON(rctx, "/api/v1/search", params, &raw); err != nil {
		return nil, err
	}

	results := sanitizeResults(raw)
	cachePut(rctx, key, results)
	return results, nil
}

// cacheGet возвращает данные только при валидном rdb: в тестах кэш отключён.
func cacheGet(rctx context.Context, key string) ([]fiber.Map, bool) {
	if rdb == nil {
		return nil, false
	}
	cctx, cancel := context.WithTimeout(rctx, 2*time.Second)
	defer cancel()

	cached, err := rdb.Get(cctx, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			log.Printf("cache get: %v", err)
		}
		return nil, false
	}
	results, ok := decodeCached(cached)
	if !ok {
		// Битая/подменённая запись не должна уходить клиенту.
		_ = rdb.Del(cctx, key).Err()
		return nil, false
	}
	return results, true
}

func cachePut(rctx context.Context, key string, results []fiber.Map) {
	if rdb == nil {
		return
	}
	data, err := json.Marshal(results)
	if err != nil {
		return
	}
	// Джиттер: пачки ключей не должны истекать синхронно.
	ttl := cacheTTL + time.Duration(rand.IntN(cacheTTLJitterSecs))*time.Second

	cctx, cancel := context.WithTimeout(rctx, 2*time.Second)
	defer cancel()
	if err := rdb.Set(cctx, key, data, ttl).Err(); err != nil {
		log.Printf("cache set: %v", err)
	}
}

func listIndexers(rctx context.Context) ([]fiber.Map, error) {
	var raw []map[string]any
	if err := prowlarrGetJSON(rctx, "/api/v1/indexer", nil, &raw); err != nil {
		return nil, err
	}

	indexers := make([]fiber.Map, 0, len(raw))
	for _, r := range raw {
		indexers = append(indexers, fiber.Map{
			"id":       asInt(r["id"]),
			"name":     cleanText(asString(r["name"]), 128),
			"protocol": cleanText(asString(r["protocol"]), 32),
			"enable":   r["enable"] == true,
		})
	}
	return indexers, nil
}

func prowlarrGetJSON(rctx context.Context, path string, params url.Values, out any) error {
	u := prowlarrURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := prowlarrHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errUpstream, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: status %d", errUpstream, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read: %v", errUpstream, err)
	}
	if len(body) > maxBodyBytes {
		return fmt.Errorf("%w: response too large", errUpstream)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: invalid json", errUpstream)
	}
	return nil
}

func sanitizeResults(raw []map[string]any) []fiber.Map {
	results := make([]fiber.Map, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))

	for _, r := range raw {
		title := cleanText(asString(r["title"]), 512)
		if title == "" {
			continue
		}

		magnet := safeMagnet(asString(r["magnetUrl"]))
		downloadURL := proxyDownloadURL(safeResultURL(asString(r["downloadUrl"])))
		if magnet == "" && downloadURL == "" {
			continue
		}
		guid := cleanText(asString(r["guid"]), 512)
		infoHash := cleanText(asString(r["infoHash"]), 128)

		key := infoHash
		if key == "" {
			key = guid
		}
		if key == "" {
			key = magnet
		}
		if key == "" {
			key = downloadURL
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		results = append(results, fiber.Map{
			"title":       title,
			"size":        asInt64(r["size"]),
			"seeders":     asInt(r["seeders"]),
			"leechers":    asInt(r["leechers"]),
			"magnet":      magnet,
			"downloadUrl": downloadURL,
			"publishDate": cleanText(asString(r["publishDate"]), 64),
			"indexer":     cleanText(asString(r["indexer"]), 128),
			"indexerId":   asInt(r["indexerId"]),
			"guid":        guid,
			"infoHash":    infoHash,
			"categories":  intSlice(r["categories"]),
		})
	}

	sort.SliceStable(results, func(i, j int) bool {
		return asInt(results[i]["seeders"]) > asInt(results[j]["seeders"])
	})
	return results
}

// proxyDownloadURL превращает ссылку Prowlarr в ссылку нашего прокси.
// Хост Prowlarr и apikey клиенту не показываются: ключ добавляет
// downloadHandler на сервере. Разрешены только пути скачивания.
func proxyDownloadURL(raw string) string {
	if raw == "" || downloadBaseURL == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !downloadPathRe.MatchString(u.EscapedPath()) {
		return ""
	}
	q := u.Query()
	q.Del("apikey")
	q.Del("api_key")
	q.Del("X-Api-Key")
	if q.Get("link") == "" {
		return ""
	}
	inner := u.EscapedPath()
	if enc := q.Encode(); enc != "" {
		inner += "?" + enc
	}
	return downloadBaseURL + "/search/download?url=" + url.QueryEscape(inner)
}

// downloadTarget проверяет путь и собирает URL Prowlarr. Хост из запроса
// клиента игнорируется — всегда prowlarrURL, поэтому подменить хост (SSRF)
// нельзя. Ключ добавляется только здесь и клиенту не возвращается.
func downloadTarget(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLen || strings.Contains(raw, "://") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "" || u.User != nil || !downloadPathRe.MatchString(u.EscapedPath()) {
		return "", false
	}
	q := u.Query()
	q.Del("apikey")
	q.Del("api_key")
	q.Del("X-Api-Key")
	if q.Get("link") == "" {
		return "", false
	}
	q.Set("apikey", apiKey)
	return prowlarrURL + u.EscapedPath() + "?" + q.Encode(), true
}

func validProxyDownloadURL(raw string) bool {
	if !strings.HasPrefix(raw, downloadBaseURL+"/search/download?") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	_, ok := downloadTarget(u.Query().Get("url"))
	return ok
}

func downloadHandler(c fiber.Ctx) error {
	target, ok := downloadTarget(c.Query("url"))
	if !ok {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid download url"})
	}

	ctx, cancel := context.WithTimeout(c.Context(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid download url"})
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Accept", "application/x-bittorrent, application/octet-stream, */*")

	resp, err := prowlarrHTTP.Do(req)
	if err != nil {
		// В ошибке Go может быть полный URL с apikey — не логируем его.
		msg := err.Error()
		if apiKey != "" {
			msg = strings.ReplaceAll(msg, apiKey, "***")
		}
		log.Printf("download: %s", msg)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "upstream unavailable"})
	}
	defer func() { _ = resp.Body.Close() }()

	// Prowlarr умеет отвечать 302 на magnet/внешний URL. Пробрасываем
	// редирект без ключа (prowlarrHTTP редиректы не выполняет).
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := strings.TrimSpace(resp.Header.Get("Location"))
		if !safeRedirectLocation(loc) {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "upstream unavailable"})
		}
		return c.Redirect().Status(fiber.StatusFound).To(loc)
	}
	if resp.StatusCode != http.StatusOK {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "upstream unavailable"})
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "upstream unavailable"})
	}
	if len(body) > maxDownloadBytes {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "download too large"})
	}

	c.Set("Content-Type", "application/x-bittorrent")
	c.Set("Content-Disposition", `attachment; filename="result.torrent"`)
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("Cache-Control", "no-store")
	return c.Send(body)
}

func safeRedirectLocation(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "magnet", "http", "https":
		return true
	default:
		return false
	}
}

func decodeCached(data []byte) ([]fiber.Map, bool) {
	var results []fiber.Map
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, false
	}
	if len(results) > maxLimit {
		return nil, false
	}
	for _, r := range results {
		if cleanText(asString(r["title"]), 512) == "" {
			return nil, false
		}
		if m := asString(r["magnet"]); m != "" && safeMagnet(m) == "" {
			return nil, false
		}
		if u := asString(r["downloadUrl"]); u != "" && !validProxyDownloadURL(u) {
			return nil, false
		}
	}
	return results, true
}

func makeCacheKey(query, categories, indexerIDs string, limit, offset int) string {
	h := sha256.New()
	// length-prefixed части: коллизий "a:b"/"b" нет.
	for _, p := range []string{
		"v3",
		strings.ToLower(strings.TrimSpace(query)),
		categories,
		indexerIDs,
		strconv.Itoa(limit),
		strconv.Itoa(offset),
	} {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return "search:torrents:v3:" + hex.EncodeToString(h.Sum(nil))
}

func parseBounded(s string, def, min, max int) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < min || v > max {
		return 0, fmt.Errorf("must be between %d and %d", min, max)
	}
	return v, nil
}

// parseIDList принимает "1,2,3" (как ждёт Prowlarr). Всё остальное — 400:
// иначе произвольная строка живёт в ключе кэша и уходит в апстрим.
func parseIDList(raw string, maxLen int) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 32 {
		return "", false
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || len(p) > 12 {
			return "", false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return "", false
		}
		out = append(out, p)
	}
	s := strings.Join(out, ",")
	if len(s) > maxLen {
		return "", false
	}
	return s, true
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func safeResultURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLen {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && blockedIP(ip) && host != prowlarrHost {
		return ""
	}
	return u.String()
}

func safeMagnet(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLen {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(raw), "magnet:?") {
		return ""
	}
	return raw
}

func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

func clampParam(s string, maxRunes int) string {
	s = truncateLog(strings.TrimSpace(s), maxRunes)
	return s
}

func cleanText(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > maxRunes {
		r = r[:maxRunes]
	}
	return strings.TrimSpace(string(r))
}

func truncateLog(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 || r == 0x1b {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func hashQuery(q string) string {
	h := sha256.Sum256([]byte(q))
	return hex.EncodeToString(h[:4])
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) int {
	return int(asInt64(v))
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	default:
		return 0
	}
}

func intSlice(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return []int{}
	}
	out := make([]int, 0, len(arr))
	for _, x := range arr {
		if len(out) >= 16 {
			break
		}
		out = append(out, asInt(x))
	}
	return out
}
