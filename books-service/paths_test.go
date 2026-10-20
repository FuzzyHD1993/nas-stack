package main

import (
	"os"
	"path/filepath"
	"testing"
)

// extractBookID принимает только строго числовые id.
func TestExtractBookIDStrict(t *testing.T) {
	good := map[string]string{
		"6013":                           "6013",
		"tag:book:6013":                  "6013",
		"https://flibusta.is/b/6013/fb2": "6013",
		"/b/6013/fb2":                    "6013",
	}
	for in, want := range good {
		if got := extractBookID(in); got != want {
			t.Errorf("extractBookID(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{
		"6013abc", "12.5", "+6013", "-6013", "abc", "../../etc/passwd", "",
	} {
		if got := extractBookID(in); got != "" {
			t.Errorf("extractBookID(%q) = %q, ожидалась пустая строка", in, got)
		}
	}
}

// resolveStoredBook не должен отдавать файл по симлинку за пределы booksDir.
func TestResolveStoredBookRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	prev := booksDir
	booksDir = dir
	t.Cleanup(func() { booksDir = prev })

	outside := filepath.Join(t.TempDir(), "secret.epub")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "evil.epub")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink not supported")
	}
	if _, err := resolveStoredBook("evil.epub"); err == nil {
		t.Fatal("симлинк за пределы booksDir прошёл")
	}

	// Обычный файл внутри booksDir по-прежнему доступен.
	ok := filepath.Join(dir, "book.epub")
	if err := os.WriteFile(ok, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveStoredBook("book.epub"); err != nil || got != ok {
		t.Fatalf("resolveStoredBook(book.epub) = %q, %v", got, err)
	}
}
