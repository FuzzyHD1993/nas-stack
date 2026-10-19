package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

var (
	mirrorMu       sync.Mutex
	activeMirror   string
	activeMirrorAt time.Time
)

// getFlibustaMirrors — список зеркал из FLIBUSTA_MIRRORS.
func getFlibustaMirrors() []string {
	raw := platform.GetEnv("FLIBUSTA_MIRRORS", "https://flibusta.is,https://flibusta.net,https://flibusta.lib")
	out := []string{}
	for _, m := range strings.Split(raw, ",") {
		m = strings.TrimSpace(m)
		if m != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		out = []string{platform.GetEnv("FLIBUSTA_URL", "https://flibusta.is")}
	}
	return out
}

// mirrorReachable — быстрая проверка доступности источника.
func mirrorReachable(base string) bool {
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get(strings.TrimRight(base, "/") + "/opds/")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == 200
}

// getFlibustaSources — локальное зеркало (приоритет) + онлайн-зеркала.
func getFlibustaSources() []string {
	out := []string{}
	if local := strings.TrimRight(platform.GetEnv("FLIBUSTA_LOCAL_URL", ""), "/"); local != "" {
		if mirrorReachable(local) {
			out = append(out, local)
			log.Printf("📚 Flibusta: локальное зеркало %s", local)
		} else {
			log.Printf("⚠️ локальное зеркало %s недоступно, fallback на онлайн", local)
		}
	}
	return append(out, getFlibustaMirrors()...)
}

// flibustaBase — первое доступное зеркало (кэш 1 час).
func flibustaBase() string {
	mirrorMu.Lock()
	defer mirrorMu.Unlock()
	if activeMirror != "" && time.Since(activeMirrorAt) < time.Hour {
		return activeMirror
	}
	hc := &http.Client{Timeout: 4 * time.Second}
	for _, m := range getFlibustaSources() {
		resp, err := hc.Head(m + "/opds")
		if err == nil && resp.StatusCode < 500 {
			_ = resp.Body.Close()
			activeMirror, activeMirrorAt = m, time.Now()
			log.Printf("📚 Flibusta mirror: %s", m)
			return m
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
	}
	log.Printf("⚠️ все зеркала Флибусты недоступны, использую %s", sourceURL)
	return sourceURL
}

// fetchWithFallback пробует все зеркала по очереди.
func fetchWithFallback(path string) (*http.Response, error) {
	hc := makeClient()
	for _, m := range getFlibustaSources() {
		req, err := newRequest("GET", m+path)
		if err != nil {
			continue
		}
		resp, err := hc.Do(req)
		if err == nil && resp.StatusCode < 500 {
			mirrorMu.Lock()
			activeMirror, activeMirrorAt = m, time.Now()
			mirrorMu.Unlock()
			return resp, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		log.Printf("⚠️ mirror %s failed", m)
	}
	return nil, fmt.Errorf("все зеркала Флибусты недоступны")
}
