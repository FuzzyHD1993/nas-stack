package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

var (
	prowlarrURL = platform.GetEnv("PROWLARR_URL", "http://host.containers.internal:9696")
	apiKey      = platform.GetEnv("PROWLARR_API_KEY", "")
	rdb         *redis.Client
	ctx         = context.Background()
)

func main() {
	if err := platform.ValidateEnv([]string{"PROWLARR_API_KEY"}); err != nil {
		log.Fatalf("❌ %v", err)
	}
	rdb = redis.NewClient(&redis.Options{
		Addr: platform.GetEnv("REDIS_ADDR", "redis:6379"),
	})

	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("search", nil)) })

	app.Get("/search", func(c fiber.Ctx) error {
		query := c.Query("q")
		if query == "" {
			return c.Status(400).JSON(fiber.Map{"error": "query is required"})
		}

		categories := c.Query("categories")
		indexerIds := c.Query("indexerIds")
		limit := parseIntDefault(c.Query("limit"), 100)
		offset := parseIntDefault(c.Query("offset"), 0)

		results, err := searchTorrents(query, categories, indexerIds, limit, offset)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		return c.JSON(fiber.Map{
			"query":   query,
			"count":   len(results),
			"limit":   limit,
			"offset":  offset,
			"results": results,
		})
	})

	app.Get("/indexers", func(c fiber.Ctx) error {
		indexers, err := listIndexers()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"indexers": indexers})
	})

	log.Println("🔍 search-service на :8088")
	log.Fatal(app.Listen(":8088"))
}

func searchTorrents(query, categories, indexerIds string, limit, offset int) ([]fiber.Map, error) {
	cacheKey := fmt.Sprintf("search:torrents:%s:%s:%s:%d:%d",
		strings.ToLower(query), categories, indexerIds, limit, offset)

	// Кэш
	if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil {
		var results []fiber.Map
		if err := json.Unmarshal([]byte(cached), &results); err == nil {
			log.Printf("💾 Кэш: %q (%d результатов)", query, len(results))
			return results, nil
		}
	}

	log.Printf("🔍 Prowlarr: %q (categories=%s, indexerIds=%s, limit=%d, offset=%d)",
		query, categories, indexerIds, limit, offset)

	params := url.Values{}
	params.Set("query", query)
	params.Set("type", "search")
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", strconv.Itoa(offset))
	if categories != "" {
		params.Set("categories", categories)
	}
	if indexerIds != "" {
		params.Set("indexerIds", indexerIds)
	}

	searchURL := fmt.Sprintf("%s/api/v1/search?%s", prowlarrURL, params.Encode())

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", apiKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prowlarr unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("prowlarr returned %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)

	var raw []map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	results := []fiber.Map{}
	for _, r := range raw {
		results = append(results, fiber.Map{
			"title":       r["title"],
			"size":        r["size"],
			"seeders":     r["seeders"],
			"leechers":    r["leechers"],
			"magnet":      r["magnetUrl"],
			"downloadUrl": r["downloadUrl"],
			"publishDate": r["publishDate"],
			"indexer":     r["indexer"],
			"indexerId":   r["indexerId"],
			"guid":        r["guid"],
			"categories":  r["categories"],
		})
	}

	// Сортировка по сидам (больше — выше)
	sort.Slice(results, func(i, j int) bool {
		si, _ := results[i]["seeders"].(float64)
		sj, _ := results[j]["seeders"].(float64)
		return si > sj
	})

	// Кэш на 1 час
	if data, err := json.Marshal(results); err == nil {
		rdb.Set(ctx, cacheKey, data, 1*time.Hour)
		log.Printf("💾 Сохранил в кэш: %q (%d)", query, len(results))
	}

	return results, nil
}

func listIndexers() ([]fiber.Map, error) {
	indexersURL := fmt.Sprintf("%s/api/v1/indexer", prowlarrURL)

	req, err := http.NewRequest("GET", indexersURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", apiKey)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prowlarr unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("prowlarr returned %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)

	var raw []map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	indexers := []fiber.Map{}
	for _, r := range raw {
		indexers = append(indexers, fiber.Map{
			"id":       r["id"],
			"name":     r["name"],
			"protocol": r["protocol"],
			"enable":   r["enable"],
		})
	}

	return indexers, nil
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}
