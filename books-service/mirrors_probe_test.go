package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestProbeMirrorRejectsNon2xx(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests} {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		t.Cleanup(srv.Close)
		t.Setenv("FLIBUSTA_MIRRORS", srv.URL)

		if probeMirror(srv.URL) {
			t.Fatalf("probeMirror(status=%d) = true, want false", code)
		}
	}
}

func TestProbeMirrorRejectsRedirectToMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FLIBUSTA_MIRRORS", srv.URL)

	if probeMirror(srv.URL) {
		t.Fatal("проба последовала редиректу во внутреннюю сеть")
	}
}

func TestGetFlibustaSourcesDedupAndNormalize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	resetSourcesCacheForTest(t)

	t.Setenv("FLIBUSTA_LOCAL_URL", srv.URL+"/")
	t.Setenv("FLIBUSTA_MIRRORS", srv.URL+", "+srv.URL+"/")

	got := getFlibustaSources()
	if len(got) != 1 {
		t.Fatalf("sources = %v, want 1 (dedupe/trim сломан)", got)
	}
	if got[0] != srv.URL {
		t.Fatalf("source = %q, want %q", got[0], srv.URL)
	}
}

func TestFetchWithFallbackSkipsNon200(t *testing.T) {
	resetMirrorCacheForTest(t)
	resetSourcesCacheForTest(t)

	var hits1, hits2 int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits1, 1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv1.Close)

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits2, 1)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv2.Close)

	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	t.Setenv("FLIBUSTA_MIRRORS", srv1.URL+","+srv2.URL)

	resp, used, err := fetchWithFallbackFrom("/book.fb2")
	if err != nil {
		t.Fatalf("fetchWithFallbackFrom: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if used != srv2.URL {
		t.Fatalf("used = %q, want %q (404 не должен обрывать fallback)", used, srv2.URL)
	}
	if atomic.LoadInt32(&hits1) != 1 || atomic.LoadInt32(&hits2) != 1 {
		t.Fatalf("hits = (%d, %d), want (1, 1)", hits1, hits2)
	}
}

func TestValidMirrorPath(t *testing.T) {
	bad := []string{"", "book.fb2", "//evil.example/x", "https://evil.example/x",
		"/a/../b", "/x\r\nHost: evil", "/x\\y"}
	for _, p := range bad {
		if validMirrorPath(p) {
			t.Errorf("validMirrorPath(%q) = true, want false", p)
		}
	}
	good := []string{"/book.fb2", "/opds/search?q=a..b", "/b/123/fb2"}
	for _, p := range good {
		if !validMirrorPath(p) {
			t.Errorf("validMirrorPath(%q) = false, want true", p)
		}
	}
}
