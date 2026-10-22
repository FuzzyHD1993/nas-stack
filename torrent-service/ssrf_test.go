package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestValidateTorrentURLRejects(t *testing.T) {
	for _, bad := range []string{
		"file:///etc/passwd",
		"gopher://127.0.0.1:6379/_INFO",
		"http://127.0.0.1:8081/add",
		"http://localhost/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/x",
		"http://user:pass@example.com/x",
		"//example.com/x",
		"",
	} {
		if _, err := validateTorrentURL(bad); err == nil {
			t.Errorf("validateTorrentURL(%q) = nil error, want reject", bad)
		}
	}
	for _, ok := range []string{"https://example.com/a.torrent", "http://example.com:8080/x"} {
		if _, err := validateTorrentURL(ok); err != nil {
			t.Errorf("validateTorrentURL(%q) = %v, want ok", ok, err)
		}
	}
}

func TestSafeHTTPClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached loopback service")
	}))
	defer srv.Close()

	if _, err := safeHTTPClient().Get(srv.URL); err == nil {
		t.Fatal("safeHTTPClient allowed loopback dial")
	}
}

func TestResolveMagnetRejectsPrivateTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if m := resolveMagnet(context.Background(), srv.URL); m != "" {
		t.Fatalf("resolveMagnet returned %q for loopback url", m)
	}
}

// TestResolveMagnetTrusted: URL из allowlist (DOWNLOAD_BASE_URL + /search/download)
// разрешён без SSRF-блока — даже если резолвится в loopback (как httptest-сервер).
func TestResolveMagnetTrusted(t *testing.T) {
	want := "magnet:?xt=urn:btih:" + strings.Repeat("0a", 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", want)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	t.Setenv("DOWNLOAD_BASE_URL", srv.URL)
	if m := resolveMagnet(context.Background(), srv.URL+"/search/download"); m != want {
		t.Fatalf("resolveMagnet = %q, want %q", m, want)
	}
}

// TestIsTrustedDownloadURL: путь allowlist'а строгий — только точное совпадение
// host:port и /search/download.
func TestIsTrustedDownloadURL(t *testing.T) {
	t.Setenv("DOWNLOAD_BASE_URL", "http://host.containers.internal:8080")
	for _, ok := range []string{"http://host.containers.internal:8080/search/download?url=x"} {
		u, err := url.Parse(ok)
		if err != nil {
			t.Fatalf("parse %q: %v", ok, err)
		}
		if !isTrustedDownloadURL(u) {
			t.Errorf("isTrustedDownloadURL(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{
		"http://host.containers.internal:8080/search/other?url=x",
		"http://host.containers.internal:8080/other/download?url=x",
		"http://evil.host:8080/search/download?url=x",
		"https://host.containers.internal:8080/search/download?url=x",
		"http://host.containers.internal:8081/search/download?url=x",
		"http://host.containers.internal:8080/search/download/evil",
	} {
		u, err := url.Parse(no)
		if err != nil {
			t.Fatalf("parse %q: %v", no, err)
		}
		if isTrustedDownloadURL(u) {
			t.Errorf("isTrustedDownloadURL(%q) = true, want false", no)
		}
	}
}

func TestValidateMagnet(t *testing.T) {
	if _, err := validateMagnet("magnet:?xt=urn:btih:abc&dn=x"); err != nil {
		t.Fatalf("valid magnet rejected: %v", err)
	}
	for _, bad := range []string{"javascript:alert(1)", "magnet:xt=urn:btih:abc", "http://x/y", ""} {
		if _, err := validateMagnet(bad); err == nil {
			t.Errorf("validateMagnet(%q) = nil error, want reject", bad)
		}
	}
}

func TestResolveDestinationRejectsTraversal(t *testing.T) {
	t.Setenv("TORRENT_DESTINATIONS", "homes/FuzzyHD/Music,homes/FuzzyHD/Movies")
	for _, bad := range []string{"../../etc", "/etc", "homes/FuzzyHD/../..", "homes/FuzzyHD/Music\x00"} {
		if _, err := resolveDestination(bad); err == nil {
			t.Errorf("resolveDestination(%q) = nil error, want reject", bad)
		}
	}
	if got, err := resolveDestination("homes/FuzzyHD/Movies"); err != nil || got != "homes/FuzzyHD/Movies" {
		t.Fatalf("allowlisted destination rejected: %q, %v", got, err)
	}
}
