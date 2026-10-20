package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

const testLocalMirror = "http://host.containers.internal:27100"

func TestGetFlibustaMirrors(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string
	}{
		{
			name: "trim and normalize",
			env:  "https://a.example, https://b.example ,https://c.example/",
			want: []string{"https://a.example", "https://b.example", "https://c.example"},
		},
		{
			name: "reject bad schemes and userinfo",
			env:  "ftp://a.example,javascript:alert(1),file:///etc/passwd,https://u:p@b.example,https://ok.example",
			want: []string{"https://ok.example"},
		},
		{
			name: "dedupe",
			env:  "https://a.example/,https://a.example",
			want: []string{"https://a.example"},
		},
		{
			name: "default when unset",
			env:  "",
			want: []string{"https://flibusta.is", "https://flibusta.net", "https://flibusta.lib"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLIBUSTA_MIRRORS", tc.env)
			t.Setenv("FLIBUSTA_URL", "https://flibusta.is")
			if got := getFlibustaMirrors(); !slices.Equal(got, tc.want) {
				t.Fatalf("mirrors = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetFlibustaMirrorsFallsBackToURL(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", " , ftp://bad.example, ")
	t.Setenv("FLIBUSTA_URL", "https://only.example")

	if got, want := getFlibustaMirrors(), []string{"https://only.example"}; !slices.Equal(got, want) {
		t.Fatalf("fallback = %v, want %v", got, want)
	}
}

func TestGetFlibustaMirrorsNeverEmpty(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", " , ftp://bad.example")
	t.Setenv("FLIBUSTA_URL", "javascript:alert(1)")

	got := getFlibustaMirrors()
	if len(got) == 0 {
		t.Fatal("getFlibustaMirrors вернул пустой список")
	}
	for _, m := range got {
		if _, ok := normalizeMirror(m); !ok {
			t.Fatalf("невалидное зеркало в результате: %q", m)
		}
	}
}

func TestGetFlibustaSourcesLocalFirstAndDedup(t *testing.T) {
	resetSourcesCacheForTest(t)

	t.Setenv("FLIBUSTA_LOCAL_URL", testLocalMirror+"/")
	t.Setenv("FLIBUSTA_MIRRORS", testLocalMirror+","+testLocalMirror+",https://a.example,https://a.example/")

	want := []string{testLocalMirror, "https://a.example"}
	if got := getFlibustaSources(); !slices.Equal(got, want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}

	// Результат не должен быть алиасом кэша.
	got := getFlibustaSources()
	got[0] = "https://mutated.example"
	if again := getFlibustaSources(); !slices.Equal(again, want) {
		t.Fatalf("кэш отдал мутированный срез: %v", again)
	}
}

func TestGetFlibustaSourcesCacheInvalidatedByEnv(t *testing.T) {
	resetSourcesCacheForTest(t)

	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	t.Setenv("FLIBUSTA_MIRRORS", "https://a.example")
	if got := getFlibustaSources(); len(got) != 1 || got[0] != "https://a.example" {
		t.Fatalf("sources = %v", got)
	}

	t.Setenv("FLIBUSTA_MIRRORS", "https://b.example")
	if got := getFlibustaSources(); len(got) != 1 || got[0] != "https://b.example" {
		t.Fatalf("кэш не инвалидировался при смене env: %v", got)
	}
}

func TestSearchPath(t *testing.T) {
	t.Setenv("FLIBUSTA_LOCAL_URL", testLocalMirror)
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is")

	const pushkin = "%D0%9F%D1%83%D1%88%D0%BA%D0%B8%D0%BD"
	cases := []struct{ name, base, query, want string }{
		{"local", testLocalMirror, "Пушкин", "/opds/search?q=" + pushkin},
		{"local trailing slash", testLocalMirror + "/", "Пушкин", "/opds/search?q=" + pushkin},
		{"flibusta", "https://flibusta.is", "Пушкин", "/opds/search?searchType=books&searchTerm=" + pushkin},
		{"flibusta trailing slash", "https://flibusta.is/", "Пушкин", "/opds/search?searchType=books&searchTerm=" + pushkin},
		{"generic", "http://other.example", "Пушкин", "/ebooks/search.opds/?query=" + pushkin},
	}
	for _, c := range cases {
		if got := searchPath(c.base, c.query); got != c.want {
			t.Errorf("%s: searchPath(%q,%q) = %q, want %q", c.name, c.base, c.query, got, c.want)
		}
	}

	// Эвристика по подстроке не должна срабатывать на чужом домене.
	if got := searchPath("https://evil-flibusta.example", "q"); got != "/ebooks/search.opds/?query=q" {
		t.Errorf("lookalike host: got %q", got)
	}

	// Инъекция параметров / CRLF / XSS.
	for _, q := range []string{
		"a&b=c",
		"a#b",
		"100%",
		"<script>alert(1)</script>",
		"a\r\nX-Injected: 1",
	} {
		want := "/ebooks/search.opds/?query=" + url.QueryEscape(q)
		got := searchPath("http://other.example", q)
		if got != want {
			t.Errorf("query %q: got %q, want %q", q, got, want)
		}
	}
}

func TestReadURL(t *testing.T) {
	t.Setenv("FLIBUSTA_LOCAL_URL", testLocalMirror)
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is")
	t.Setenv("FLIBUSTA_URL", "https://flibusta.is")

	cases := []struct{ name, base, id, want string }{
		{"local numeric", testLocalMirror, "200000", testLocalMirror + "/fb2.php?id=200000"},
		{"local base with slash", testLocalMirror + "/", "200000", testLocalMirror + "/fb2.php?id=200000"},
		{"online tag id", "https://flibusta.is", "tag:book:200000", "https://flibusta.is/b/200000/fb2"},
		{"absolute path", "https://flibusta.is", "/b/1/fb2", "https://flibusta.is/b/1/fb2"},
		{"allowed absolute", "https://flibusta.is", "https://flibusta.is/b/1/fb2", "https://flibusta.is/b/1/fb2"},
		{"foreign absolute", "https://flibusta.is", "https://x/y.fb2", ""},
		{"metadata absolute", "https://flibusta.is", "http://169.254.169.254/latest/meta-data/", ""},
		{"uppercase unsafe absolute", "https://flibusta.is", "HTTP://169.254.169.254/", ""},
		{"userinfo absolute", "https://flibusta.is", "https://flibusta.is@127.0.0.1/x", ""},
		{"file scheme", "https://flibusta.is", "file:///etc/passwd", ""},
		{"no digits", "https://flibusta.is", "tag:book:abc", ""},
		{"empty id", "https://flibusta.is", "", ""},
		{"protocol relative stays on mirror", testLocalMirror, "//evil.example/x", testLocalMirror + "//evil.example/x"},
	}
	for _, c := range cases {
		if got := readURL(c.base, c.id); got != c.want {
			t.Errorf("%s: readURL(%q,%q) = %q, want %q", c.name, c.base, c.id, got, c.want)
		}
	}
}

func TestFlibustaBaseConcurrent(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	resetHTTPClientForTest(t)
	resetSourcesCacheForTest(t)
	resetMirrorCacheForTest(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/opds" || r.URL.Path == "/opds/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("FLIBUSTA_MIRRORS", srv.URL)
	t.Setenv("FLIBUSTA_LOCAL_URL", "")

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = flibustaBase()
		}()
	}
	wg.Wait()

	mirrorMu.Lock()
	got := activeMirror
	mirrorMu.Unlock()
	if got != srv.URL {
		t.Fatalf("activeMirror = %q, want %q", got, srv.URL)
	}
}

func TestMirrorProbeRejectsInternalRedirect(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	resetHTTPClientForTest(t)
	resetMirrorCacheForTest(t)

	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(internal.Close)

	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(redir.Close)

	t.Setenv("FLIBUSTA_MIRRORS", redir.URL)
	t.Setenv("FLIBUSTA_LOCAL_URL", "")

	if mirrorReachable(redir.URL) {
		t.Fatal("зеркало с redirect во внутреннюю сеть признано доступным")
	}
	if n := internalHits.Load(); n != 0 {
		t.Fatalf("redirect во внутреннюю сеть выполнен: %d запросов", n)
	}
}
