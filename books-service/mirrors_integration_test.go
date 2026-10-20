//go:build integration

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func prepareIntegrationMirrors(t *testing.T, local string, mirrors ...string) {
	t.Helper()

	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("FLIBUSTA_LOCAL_URL", local)
	t.Setenv("FLIBUSTA_MIRRORS", strings.Join(mirrors, ","))

	resetHTTPClientForTest(t)
	resetSourcesCacheForTest(t)
	resetMirrorCacheForTest(t)
}

// TestFetchWithFallbackIntegration поднимает локальное "зеркало" и проверяет,
// что fetchWithFallback ходит в него, а не в интернет.
func TestFetchWithFallbackIntegration(t *testing.T) {
	const body = `<?xml version="1.0" encoding="utf-8"?><FictionBook><body>ok</body></FictionBook>`

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/opds", "/opds/":
			w.WriteHeader(http.StatusOK)
		case "/book.fb2":
			hits.Add(1)
			w.Header().Set("Content-Type", "application/x-fictionbook+xml")
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// Sentinel: если локальное зеркало не будет опрошено, тест упадёт быстро,
	// а не уйдёт в интернет.
	prepareIntegrationMirrors(t, srv.URL, "http://127.0.0.1:1")

	resp, used, err := fetchWithFallbackFrom("/book.fb2")
	if err != nil {
		t.Fatalf("fetchWithFallbackFrom: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if used != srv.URL {
		t.Fatalf("used = %q, want %q", used, srv.URL)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Request.URL.Host; got != strings.TrimPrefix(srv.URL, "http://") {
		t.Fatalf("запрос ушёл на %q", got)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != body {
		t.Fatalf("body = %q, want %q", data, body)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

func TestFetchWithFallbackUsesNextMirror(t *testing.T) {
	const body = `<?xml version="1.0"?><FictionBook><body>ok</body></FictionBook>`

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)

	var goodHits atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/book.fb2" {
			http.NotFound(w, r)
			return
		}
		goodHits.Add(1)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(good.Close)

	prepareIntegrationMirrors(t, bad.URL, good.URL)

	resp, used, err := fetchWithFallbackFrom("/book.fb2")
	if err != nil {
		t.Fatalf("fetchWithFallbackFrom: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if used != good.URL {
		t.Fatalf("used = %q, want %q", used, good.URL)
	}
	if goodHits.Load() == 0 {
		t.Fatal("fallback-зеркало не было опрошено")
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(data) != body {
		t.Fatalf("body = %q, want %q", data, body)
	}
}

func TestMirrorReachableDoesNotFollowRedirectToInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	prepareIntegrationMirrors(t, srv.URL, srv.URL)

	if mirrorReachable(srv.URL) {
		t.Fatal("проба последовала редиректу во внутреннюю сеть")
	}
}
