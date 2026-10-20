package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

const (
	mirrorTTL          = time.Hour
	mirrorFailureTTL   = 30 * time.Second
	mirrorProbeTimeout = 4 * time.Second
	mirrorSourcesTTL   = 30 * time.Second

	defaultMirrorList = "https://flibusta.is,https://flibusta.net,https://flibusta.lib"
)

var (
	mirrorMu       sync.Mutex
	activeMirror   string
	activeMirrorAt time.Time
	mirrorFailedAt time.Time

	// probeMu сериализует перебор зеркал: параллельные запросы не должны
	// запускать N полных сканов одновременно.
	probeMu sync.Mutex

	sourcesMu    sync.Mutex
	sourcesList  []string
	sourcesAt    time.Time
	sourcesKeyAt string
)

// normalizeMirror — единственная точка нормализации адреса зеркала:
// только http/https, без userinfo/query/fragment и хвостового слэша.
func normalizeMirror(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Hostname() == "" {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", false
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	return strings.TrimRight(u.String(), "/"), true
}

// redactURL скрывает userinfo, path и query: креды и внутренняя топология
// не должны попадать в логи и /health.
func redactURL(raw string) string {
	raw = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<invalid-url>"
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host
}

// getFlibustaMirrors — список зеркал из FLIBUSTA_MIRRORS
// (нормализованный, без дублей, никогда не пустой).
func getFlibustaMirrors() []string {
	raw := platform.GetEnv("FLIBUSTA_MIRRORS", defaultMirrorList)

	out := []string{}
	seen := map[string]struct{}{}
	add := func(v string) {
		m, ok := normalizeMirror(v)
		if !ok {
			return
		}
		if _, dup := seen[m]; dup {
			return
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	for _, m := range strings.Split(raw, ",") {
		add(m)
	}
	if len(out) == 0 {
		add(platform.GetEnv("FLIBUSTA_URL", "https://flibusta.is"))
	}
	if len(out) == 0 {
		// Даже FLIBUSTA_URL невалиден: пустой список ломает fallback.
		for _, m := range strings.Split(defaultMirrorList, ",") {
			add(m)
		}
	}
	return out
}

func sourcesCacheKey() string {
	return strings.Join([]string{
		platform.GetEnv("FLIBUSTA_LOCAL_URL", ""),
		platform.GetEnv("FLIBUSTA_URL", ""),
		strings.Join(getFlibustaMirrors(), ","),
	}, "|")
}

// getFlibustaSources — кандидаты на опрос: локальное зеркало первым, затем
// онлайн. Сетевых проб здесь нет: доступность определяет flibustaBaseOK,
// иначе /read и /search платят за probe на каждом вызове.
func getFlibustaSources() []string {
	key := sourcesCacheKey()

	sourcesMu.Lock()
	if sourcesList != nil && key == sourcesKeyAt && time.Since(sourcesAt) < mirrorSourcesTTL {
		out := append([]string(nil), sourcesList...)
		sourcesMu.Unlock()
		return out
	}
	sourcesMu.Unlock()

	out := []string{}
	seen := map[string]struct{}{}
	add := func(raw string) {
		m, ok := normalizeMirror(raw)
		if !ok {
			return
		}
		if _, dup := seen[m]; dup {
			return
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	add(platform.GetEnv("FLIBUSTA_LOCAL_URL", ""))
	for _, m := range getFlibustaMirrors() {
		add(m)
	}
	if len(out) == 0 {
		add(platform.GetEnv("FLIBUSTA_URL", sourceURL))
	}

	sourcesMu.Lock()
	sourcesList = append([]string(nil), out...)
	sourcesAt = time.Now()
	sourcesKeyAt = key
	sourcesMu.Unlock()
	return out
}

// orderedSources — активное зеркало первым: тёплый кэш не должен платить
// за мёртвое локальное зеркало.
func orderedSources() []string {
	sources := getFlibustaSources()
	cached, ok := cachedMirror()
	if !ok || cached == "" {
		return sources
	}
	out := make([]string, 0, len(sources)+1)
	out = append(out, cached)
	for _, s := range sources {
		if s != cached {
			out = append(out, s)
		}
	}
	return out
}

// probeClient переиспользует транспорт и CheckRedirect из makeClient
// (SSRF-политика, pinnedDialContext, proxy), но с коротким таймаутом.
func probeClient(timeout time.Duration) *http.Client {
	base := makeClient()
	return &http.Client{
		Timeout:       timeout,
		Transport:     base.Transport,
		CheckRedirect: base.CheckRedirect,
	}
}

func drainClose(resp *http.Response, limit int64) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
	_ = resp.Body.Close()
}

// probeMirror — зеркало живо только при 2xx на /opds. 4xx (403/404/429) и
// ошибки редиректа означают «не годится» и не должны кэшироваться на час.
func probeMirror(base string) bool {
	base, ok := normalizeMirror(base)
	if !ok {
		return false
	}
	hc := probeClient(mirrorProbeTimeout)
	target := base + "/opds"

	req, err := newRequest(http.MethodHead, target)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		// При "stopped after N redirects" resp может быть не nil.
		drainClose(resp, 4<<10)
		return false
	}
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		drainClose(resp, 4<<10)
		req, err = newRequest(http.MethodGet, target)
		if err != nil {
			return false
		}
		resp, err = hc.Do(req)
		if err != nil {
			drainClose(resp, 4<<10)
			return false
		}
	}
	defer drainClose(resp, 4<<10)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// mirrorReachable — проба через общую SSRF-политику makeClient.
func mirrorReachable(base string) bool {
	m, ok := normalizeMirror(base)
	return ok && probeMirror(m)
}

// cachedMirror — без сетевых вызовов (для /health).
func cachedMirror() (string, bool) {
	mirrorMu.Lock()
	defer mirrorMu.Unlock()
	if activeMirror != "" && time.Since(activeMirrorAt) < mirrorTTL {
		return activeMirror, true
	}
	return sourceURL, false
}

// flibustaBaseOK — (base, ok); ok=false означает «рабочих зеркал нет».
func flibustaBaseOK() (string, bool) {
	if m, ok := cachedMirror(); ok {
		return m, true
	}

	mirrorMu.Lock()
	recentFailure := !mirrorFailedAt.IsZero() && time.Since(mirrorFailedAt) < mirrorFailureTTL
	mirrorMu.Unlock()
	if recentFailure {
		return "", false
	}

	probeMu.Lock()
	defer probeMu.Unlock()

	// double-check: пока ждали probeMu, другой goroutine мог прогреть кэш.
	if m, ok := cachedMirror(); ok {
		return m, true
	}

	base, ok := probeMirrors()

	mirrorMu.Lock()
	if ok {
		activeMirror, activeMirrorAt, mirrorFailedAt = base, time.Now(), time.Time{}
	} else {
		activeMirror, activeMirrorAt = "", time.Time{}
		mirrorFailedAt = time.Now()
	}
	mirrorMu.Unlock()

	if ok {
		log.Printf("📚 Flibusta mirror: %s", redactURL(base))
	} else {
		log.Printf("⚠️ все зеркала Флибусты недоступны")
	}
	return base, ok
}

// flibustaBase оставлен для построения URL. При отказе возвращает sourceURL
// как «последнюю надежду»; /health использует cachedMirror.
func flibustaBase() string {
	if b, ok := flibustaBaseOK(); ok {
		return b
	}
	return sourceURL
}

// probeMirrors вызывается под probeMu.
func probeMirrors() (string, bool) {
	for _, m := range getFlibustaSources() {
		if probeMirror(m) {
			return m, true
		}
		log.Printf("⚠️ зеркало %s недоступно", redactURL(m))
	}
	return "", false
}

func markMirrorOK(m string) {
	mirrorMu.Lock()
	activeMirror, activeMirrorAt, mirrorFailedAt = m, time.Now(), time.Time{}
	mirrorMu.Unlock()
}

// validMirrorPath — путь относительный и не выходит за пределы зеркала:
// "//host", scheme, CRLF, "\" и ".."-сегменты запрещены.
func validMirrorPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return false
	}
	if strings.ContainsAny(path, "\r\n\\\x00") {
		return false
	}
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// fetchWithFallback пробует все зеркала по очереди (путь фиксированный).
func fetchWithFallback(path string) (*http.Response, error) {
	resp, _, err := fetchWithFallbackFrom(path)
	return resp, err
}

// fetchWithFallbackFrom возвращает и ответ, и зеркало, которое ответило:
// относительные ссылки нельзя склеивать с другим (возможно мёртвым) зеркалом.
func fetchWithFallbackFrom(path string) (*http.Response, string, error) {
	return fetchWithFallbackFunc(func(string) string { return path })
}

// fetchWithFallbackFunc перебирает зеркала; pathFor строит путь под конкретное
// зеркало (форматы OPDS у локального и онлайн-зеркал разные).
// Успех — только 2xx: 404 от первого зеркала не должен обрывать fallback.
func fetchWithFallbackFunc(pathFor func(base string) string) (*http.Response, string, error) {
	hc := makeClient()
	var errs []error

	for _, m := range orderedSources() {
		path := pathFor(m)
		if !validMirrorPath(path) {
			errs = append(errs, fmt.Errorf("%s: invalid path %q", redactURL(m), path))
			continue
		}
		req, err := newRequest(http.MethodGet, m+path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", redactURL(m), err))
			continue
		}
		resp, err := hc.Do(req)
		if err != nil {
			drainClose(resp, 64<<10)
			errs = append(errs, fmt.Errorf("%s: %w", redactURL(m), err))
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			status := resp.StatusCode
			drainClose(resp, 64<<10)
			errs = append(errs, fmt.Errorf("%s: status %d", redactURL(m), status))
			continue
		}

		markMirrorOK(m)
		return resp, m, nil
	}

	if len(errs) == 0 {
		errs = append(errs, errors.New("no mirror sources"))
	}
	return nil, "", fmt.Errorf("все зеркала Флибусты недоступны: %w", errors.Join(errs...))
}
