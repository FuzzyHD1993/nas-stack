package main

import (
	"net/url"
	"testing"
)

func TestGetFlibustaMirrors(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://a.example,https://b.example, https://c.example")
	got := getFlibustaMirrors()
	if len(got) != 3 {
		t.Fatalf("mirrors = %v", got)
	}
	if got[0] != "https://a.example" || got[2] != "https://c.example" {
		t.Errorf("unexpected mirrors: %v", got)
	}
}

func TestGetFlibustaSourcesWithoutLocal(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://a.example,https://b.example")
	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	got := getFlibustaSources()
	if len(got) != 2 || got[0] != "https://a.example" {
		t.Fatalf("sources = %v", got)
	}
}

func TestGetFlibustaMirrorsDefault(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "")
	got := getFlibustaMirrors()
	if len(got) != 3 {
		t.Fatalf("default mirrors = %v", got)
	}
}

func TestSearchPath(t *testing.T) {
	t.Setenv("FLIBUSTA_LOCAL_URL", "http://host.containers.internal:27100")
	q := url.QueryEscape("Пушкин")
	cases := []struct{ base, want string }{
		{"http://host.containers.internal:27100", "/opds/search?q=" + q},
		{"https://flibusta.is", "/opds/search?searchType=books&searchTerm=" + q},
		{"http://other.example", "/ebooks/search.opds/?query=" + q},
	}
	for _, c := range cases {
		if got := searchPath(c.base, "Пушкин"); got != c.want {
			t.Errorf("searchPath(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

func TestReadURL(t *testing.T) {
	t.Setenv("FLIBUSTA_LOCAL_URL", "http://host.containers.internal:27100")
	if got := readURL("http://host.containers.internal:27100", "200000"); got != "http://host.containers.internal:27100/fb2.php?id=200000" {
		t.Errorf("local readURL = %q", got)
	}
	if got := readURL("https://flibusta.is", "tag:book:200000"); got != "https://flibusta.is/b/200000/fb2" {
		t.Errorf("online readURL = %q", got)
	}
	if got := readURL("https://flibusta.is", "https://x/y.fb2"); got != "https://x/y.fb2" {
		t.Errorf("absolute readURL = %q", got)
	}
}
