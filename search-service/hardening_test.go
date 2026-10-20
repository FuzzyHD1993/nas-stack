package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseBounded(t *testing.T) {
	cases := []struct {
		in       string
		def      int
		min, max int
		want     int
		wantErr  bool
	}{
		{"", 100, 1, 100, 100, false},
		{"50", 100, 1, 100, 50, false},
		{"0", 100, 1, 100, 0, true},
		{"-3", 0, 0, 10000, 0, true},
		{"100000", 0, 0, 10000, 0, true},
		{"abc", 100, 1, 100, 0, true},
	}
	for _, c := range cases {
		got, err := parseBounded(c.in, c.def, c.min, c.max)
		if (err != nil) != c.wantErr {
			t.Errorf("parseBounded(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseBounded(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseIDList(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"1", "1", true},
		{"1, 2 ,3", "1,2,3", true},
		{"1,x", "", false},
		{"1,,2", "", false},
		{"../../etc/passwd", "", false},
		{"http://evil", "", false},
		{strings.Repeat("1", 13), "", false},
	}
	for _, tc := range cases {
		got, ok := parseIDList(tc.in, maxParamLen)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseIDList(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMakeCacheKeyNoCollision(t *testing.T) {
	a := makeCacheKey("a:b", "c", "d", 100, 0)
	b := makeCacheKey("a", "b:c", "d", 100, 0)
	if a == b {
		t.Fatal("коллизия ключа кэша")
	}
	if a != makeCacheKey(" A:B ", "c", "d", 100, 0) {
		t.Error("ключ должен быть стабилен для нормализованного query")
	}
}

func TestSafeResultURL(t *testing.T) {
	bad := []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"data:text/html,x",
		"http://127.0.0.1/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://10.0.0.1/x",
		"http://user:pass@tracker.example/x",
		"http://localhost/x",
	}
	for _, u := range bad {
		if got := safeResultURL(u); got != "" {
			t.Errorf("safeResultURL(%q) = %q, want empty", u, got)
		}
	}
	good := "https://tracker.example/download/1"
	if got := safeResultURL(" " + good + " "); got != good {
		t.Errorf("safeResultURL(%q) = %q", good, got)
	}
}

func TestSafeMagnet(t *testing.T) {
	if safeMagnet("javascript:alert(1)") != "" {
		t.Error("javascript: принят как magnet")
	}
	if safeMagnet("http://evil/x") != "" {
		t.Error("http URL принят как magnet")
	}
	if safeMagnet("magnet:?xt=urn:btih:abc") == "" {
		t.Error("валидный magnet отвергнут")
	}
}

func TestSanitizeResults(t *testing.T) {
	raw := []map[string]any{
		{
			"title":       "Good\x00 torrent",
			"size":        float64(1024),
			"seeders":     float64(5),
			"leechers":    float64(1),
			"magnetUrl":   "magnet:?xt=urn:btih:abc",
			"downloadUrl": "javascript:alert(1)",
			"guid":        "g1",
			"categories":  []any{float64(2000), float64(5000)},
		},
		{
			"title":     "Duplicate guid",
			"magnetUrl": "magnet:?xt=urn:btih:def",
			"guid":      "g1",
		},
		{
			"title":       "No links",
			"magnetUrl":   "",
			"downloadUrl": "",
		},
		{
			"title":       "",
			"downloadUrl": "https://tracker.example/x",
		},
		{
			"title":       "URL only",
			"downloadUrl": "https://tracker.example/dl/2",
			"seeders":     float64(10),
		},
	}

	out := sanitizeResults(raw)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2: %#v", len(out), out)
	}
	// Сортировка по сидам: URL only (10) выше Good torrent (5).
	if out[0]["title"] != "URL only" || !strings.HasPrefix(out[1]["title"].(string), "Good") {
		t.Fatalf("unexpected order: %#v", out)
	}
	if out[1]["downloadUrl"] != "" {
		t.Fatalf("javascript: URL не отфильтрован: %v", out[1]["downloadUrl"])
	}
	if !strings.HasPrefix(out[1]["title"].(string), "Good") {
		t.Fatalf("unexpected title: %q", out[1]["title"])
	}
	if strings.Contains(out[1]["title"].(string), "\x00") {
		t.Fatalf("control-символ остался в title: %q", out[1]["title"])
	}
	if got := out[1]["categories"].([]int); len(got) != 2 || got[0] != 2000 || got[1] != 5000 {
		t.Fatalf("categories = %v", got)
	}
}

func TestCleanTextAndClampParam(t *testing.T) {
	if got := cleanText("  a\nb\x1b  ", 10); got != "a b" {
		t.Fatalf("cleanText = %q", got)
	}
	if got := clampParam("  "+strings.Repeat("x", 300), 10); len([]rune(got)) != 11 {
		// 10 символов + многоточие
		t.Fatalf("clampParam len = %d (%q)", len([]rune(got)), got)
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"http://Host.Example:9696/base": "host.example",
		"https://example.org":           "example.org",
		"file:///etc/passwd":            "",
		"://bad":                        "",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProxyParamsStayEncoded(t *testing.T) {
	params := url.Values{}
	params.Set("query", "a&b=c d")
	if got := params.Encode(); !strings.Contains(got, "%26") || !strings.Contains(got, "+") {
		t.Fatalf("query не закодирован: %s", got)
	}
}
