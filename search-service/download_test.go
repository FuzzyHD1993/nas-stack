package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestDownloadTarget(t *testing.T) {
	oldURL, oldKey := prowlarrURL, apiKey
	defer func() { prowlarrURL, apiKey = oldURL, oldKey }()
	prowlarrURL, apiKey = "http://prowlarr:9696", "test-key"

	for _, bad := range []string{
		"",
		"http://evil.example/2/download?link=x",
		"//evil.example/2/download?link=x",
		"/2/download",
		"/2/download?link=",
		"/api/v1/system/status?link=x",
		"/../2/download?link=x",
		"/2/download/extra?link=x",
	} {
		if got, ok := downloadTarget(bad); ok {
			t.Errorf("downloadTarget(%q) = %q, want reject", bad, got)
		}
	}

	for _, good := range []string{
		"/2/download?link=abc",
		"/2/download?link=abc&apikey=evil",
		"/api/v1/indexer/3/download?link=abc&file=x",
	} {
		got, ok := downloadTarget(good)
		if !ok {
			t.Fatalf("downloadTarget(%q) отвергнут", good)
		}
		if !strings.HasPrefix(got, "http://prowlarr:9696/") {
			t.Fatalf("downloadTarget(%q) = %q, чужой хост", good, got)
		}
		if strings.Contains(got, "evil") {
			t.Fatalf("downloadTarget(%q) = %q, клиентский ключ не вырезан", good, got)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("downloadTarget(%q) = %q: %v", good, got, err)
		}
		if u.Query().Get("apikey") != "test-key" {
			t.Fatalf("downloadTarget(%q): apikey = %q, want test-key", good, u.Query().Get("apikey"))
		}
	}
}

func TestDownloadHandlerStreamsWithKey(t *testing.T) {
	oldURL, oldKey, oldBase, oldHTTP := prowlarrURL, apiKey, downloadBaseURL, prowlarrHTTP
	defer func() {
		prowlarrURL, apiKey, downloadBaseURL, prowlarrHTTP = oldURL, oldKey, oldBase, oldHTTP
	}()
	apiKey = "test-key"
	downloadBaseURL = "http://gateway:8080"

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/2/download" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("link") != "abc" {
			t.Errorf("link = %q", r.URL.Query().Get("link"))
		}
		if r.URL.Query().Get("apikey") != "test-key" {
			t.Errorf("apikey = %q", r.URL.Query().Get("apikey"))
		}
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("X-Api-Key = %q", r.Header.Get("X-Api-Key"))
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write([]byte("torrent-bytes"))
	}))
	defer srv.Close()
	prowlarrURL = srv.URL

	app := fiber.New()
	app.Get("/download", downloadHandler)

	req := httptest.NewRequest(http.MethodGet, "/download?url="+url.QueryEscape("/2/download?link=abc"), nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if string(body) != "torrent-bytes" {
		t.Fatalf("body = %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-bittorrent" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestDownloadHandlerRejectsWithoutUpstream(t *testing.T) {
	oldURL, oldKey, oldBase := prowlarrURL, apiKey, downloadBaseURL
	defer func() { prowlarrURL, apiKey, downloadBaseURL = oldURL, oldKey, oldBase }()
	apiKey = "test-key"
	downloadBaseURL = "http://gateway:8080"

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()
	prowlarrURL = srv.URL

	app := fiber.New()
	app.Get("/download", downloadHandler)

	for _, bad := range []string{
		"http://evil.example/2/download?link=x",
		"//evil.example/2/download?link=x",
		"/2/download",
		"/api/v1/system/status?link=x",
	} {
		req := httptest.NewRequest(http.MethodGet, "/download?url="+url.QueryEscape(bad), nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test(%q): %v", bad, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("downloadTarget(%q): status = %d, want 400", bad, resp.StatusCode)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("upstream вызван %d раз при невалидных URL", n)
	}
}

func TestProxyDownloadURL(t *testing.T) {
	oldBase := downloadBaseURL
	defer func() { downloadBaseURL = oldBase }()
	downloadBaseURL = "http://gateway:8080"

	got := proxyDownloadURL("http://prowlarr:9696/2/download?apikey=secret&link=abc&file=My+Movie")
	if got == "" {
		t.Fatal("валидная ссылка отвергнута")
	}
	if strings.Contains(got, "secret") || strings.Contains(strings.ToLower(got), "apikey") {
		t.Fatalf("ключ утёк: %q", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	inner := u.Query().Get("url")
	if strings.HasPrefix(inner, "http") || strings.Contains(strings.ToLower(inner), "apikey") {
		t.Fatalf("внутренний url = %q", inner)
	}
	if _, ok := downloadTarget(inner); !ok {
		t.Fatalf("внутренний url не проходит downloadTarget: %q", inner)
	}

	for _, bad := range []string{
		"http://prowlarr:9696/2/download?apikey=secret", // нет link
		"http://prowlarr:9696/api/v1/search?link=abc",   // не download
		"javascript:alert(1)",
	} {
		if v := proxyDownloadURL(bad); v != "" {
			t.Errorf("proxyDownloadURL(%q) = %q, want empty", bad, v)
		}
	}

	downloadBaseURL = ""
	if v := proxyDownloadURL("http://prowlarr:9696/2/download?link=abc"); v != "" {
		t.Errorf("пустой DOWNLOAD_BASE_URL: got %q, want empty", v)
	}
}

func TestDownloadHandlerForwardsMagnetRedirect(t *testing.T) {
	oldURL, oldKey, oldBase := prowlarrURL, apiKey, downloadBaseURL
	defer func() { prowlarrURL, apiKey, downloadBaseURL = oldURL, oldKey, oldBase }()
	apiKey = "test-key"
	downloadBaseURL = "http://gateway:8080"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "magnet:?xt=urn:btih:abc&dn=x")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	prowlarrURL = srv.URL

	app := fiber.New()
	app.Get("/download", downloadHandler)

	req := httptest.NewRequest(http.MethodGet, "/download?url="+url.QueryEscape("/2/download?link=abc"), nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "magnet:?xt=urn:btih:abc&dn=x" {
		t.Fatalf("Location = %q", got)
	}
}

func TestDownloadHandlerRejectsOversized(t *testing.T) {
	oldURL, oldKey, oldBase := prowlarrURL, apiKey, downloadBaseURL
	defer func() { prowlarrURL, apiKey, downloadBaseURL = oldURL, oldKey, oldBase }()
	apiKey = "test-key"
	downloadBaseURL = "http://gateway:8080"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxDownloadBytes+1))
	}))
	defer srv.Close()
	prowlarrURL = srv.URL

	app := fiber.New()
	app.Get("/download", downloadHandler)

	req := httptest.NewRequest(http.MethodGet, "/download?url="+url.QueryEscape("/2/download?link=abc"), nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestDecodeCachedDownloadURL(t *testing.T) {
	oldBase := downloadBaseURL
	defer func() { downloadBaseURL = oldBase }()
	downloadBaseURL = "http://gateway:8080"

	old := []byte(`[{"title":"x","downloadUrl":"http://host.containers.internal:9696/2/download?apikey=super-secret&link=abc"}]`)
	if _, ok := decodeCached(old); ok {
		t.Fatal("старый downloadUrl с apikey принят из кэша")
	}

	good := downloadBaseURL + "/search/download?url=" + url.QueryEscape("/2/download?link=abc")
	fresh := []byte(`[{"title":"x","downloadUrl":"` + good + `"}]`)
	if _, ok := decodeCached(fresh); !ok {
		t.Fatal("валидная прокси-ссылка отвергнута")
	}
}
