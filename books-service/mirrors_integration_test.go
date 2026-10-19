//go:build integration

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchWithFallbackIntegration поднимает локальное "зеркало" и проверяет,
// что fetchWithFallback ходит в него, а не в интернет.
func TestFetchWithFallbackIntegration(t *testing.T) {
	const body = `<?xml version="1.0" encoding="utf-8"?><FictionBook><body>ok</body></FictionBook>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/opds/":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "opds")
		case "/book.fb2":
			w.Header().Set("Content-Type", "application/x-fictionbook+xml")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("FLIBUSTA_LOCAL_URL", srv.URL)
	t.Setenv("FLIBUSTA_MIRRORS", srv.URL)
	// В тесте не должен использоваться корпоративный/системный HTTP-прокси.
	oldProxy := httpProxy
	httpProxy = ""
	defer func() { httpProxy = oldProxy }()

	resp, err := fetchWithFallback("/book.fb2")
	if err != nil {
		t.Fatalf("fetchWithFallback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(data), "FictionBook") {
		t.Fatalf("unexpected body: %q", string(data))
	}
}
