package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// primeMirror кэширует «активное зеркало», чтобы тесты не ходили в сеть.
func primeMirror(t *testing.T) {
	t.Helper()
	mirrorMu.Lock()
	defer mirrorMu.Unlock()
	prevM, prevAt := activeMirror, activeMirrorAt
	activeMirror, activeMirrorAt = "https://flibusta.is", time.Now()
	t.Cleanup(func() {
		mirrorMu.Lock()
		defer mirrorMu.Unlock()
		activeMirror, activeMirrorAt = prevM, prevAt
	})
}

// --- Фикс 1: whitelist формата (path traversal через format) ---

func TestNormalizeFormatWhitelist(t *testing.T) {
	ok := []string{"epub", "fb2", "mobi", "txt", "EPUB", " EPUB ", "Fb2"}
	for _, f := range ok {
		got, err := normalizeFormat(f)
		if err != nil {
			t.Fatalf("normalizeFormat(%q) неожиданная ошибка: %v", f, err)
		}
		if strings.TrimSpace(strings.ToLower(f)) != got {
			t.Fatalf("normalizeFormat(%q) = %q", f, got)
		}
	}

	bad := []string{
		"../../../../tmp/pwned",
		"epub/../../etc/passwd",
		"..",
		".",
		"zip",
		"",
		"epub.epub",
		"/tmp/x",
		"epub\x00txt",
		"тхт",
	}
	for _, f := range bad {
		got, err := normalizeFormat(f)
		if err == nil {
			t.Errorf("normalizeFormat(%q) = %q, ожидалась ошибка", f, got)
			continue
		}
		if !errors.Is(err, errBadRequest) {
			t.Errorf("normalizeFormat(%q): ошибка не обёрнута в errBadRequest: %v", f, err)
		}
	}
}

// safeBookPath: файл обязан остаться внутри booksDir.
func TestSafeBookPathStaysInsideDir(t *testing.T) {
	dir := t.TempDir()
	prev := booksDir
	booksDir = dir
	t.Cleanup(func() { booksDir = prev })

	dest, err := safeBookPath("Война и мир", "epub")
	if err != nil {
		t.Fatalf("safeBookPath: %v", err)
	}
	if filepath.Dir(dest) != dir {
		t.Fatalf("файл вне booksDir: %s (booksDir=%s)", dest, dir)
	}
	if filepath.Base(dest) != "Война и мир.epub" {
		t.Fatalf("имя файла = %q", filepath.Base(dest))
	}

	// Точка-имя, слеши, NUL — всё отсекается (sanitizeFilename + проверка пути).
	for _, title := range []string{
		"../../etc/passwd",
		"..",
		"a/b",
		"\x00evil",
		"",
		"   ",
	} {
		dest, err := safeBookPath(title, "epub")
		if err != nil {
			continue // отклонено — хорошо
		}
		rel, relErr := filepath.Rel(dir, dest)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			t.Errorf("safeBookPath(%q) = %q вышел за пределы booksDir", title, dest)
		}
		if strings.ContainsRune(filepath.Base(dest), 0) {
			t.Errorf("safeBookPath(%q) = %q: NUL не отсечён", title, dest)
		}
		if filepath.Dir(dest) != dir {
			t.Errorf("safeBookPath(%q) = %q: каталог отличается от booksDir", title, dest)
		}
	}
}

// Интеграционный сценарий уязвимости: format с traversal не должен
// приводить к записи файла за пределами booksDir.
func TestDownloadBookRejectsTraversalFormat(t *testing.T) {
	dir := t.TempDir()
	prevDir := booksDir
	booksDir = dir
	t.Cleanup(func() { booksDir = prevDir })
	primeMirror(t)

	err := downloadBook("123", "Книга", "../../../../tmp/pwned")
	if !errors.Is(err, errBadRequest) {
		t.Fatalf("downloadBook принял traversal-формат: %v", err)
	}
	if _, statErr := os.Stat("/tmp/pwned"); statErr == nil {
		t.Fatal("!!! создан файл /tmp/pwned")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("в booksDir появились файлы: %v", entries)
	}
}

// --- Фикс 2: SSRF (id с произвольным URL) ---

func TestResolveDownloadURLRejectsForeignHosts(t *testing.T) {
	primeMirror(t)

	bad := []string{
		"http://attacker.example/evil.epub",
		"https://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:8080/",
		"http://localhost:8083/env",
		"http://192.168.0.1/share.epub",
		"http://10.0.0.5/",
		"file:///etc/passwd",
		"ftp://flibusta.is/x.epub",
		"gopher://flibusta.is:70/x",
	}
	for _, id := range bad {
		got, err := resolveDownloadURL(id, "epub")
		if err == nil {
			t.Errorf("resolveDownloadURL(%q) = %q, ожидался отказ (SSRF)", id, got)
			continue
		}
		if !errors.Is(err, errBadRequest) {
			t.Errorf("resolveDownloadURL(%q): не errBadRequest: %v", id, err)
		}
	}
}

func TestResolveDownloadURLAcceptsTrusted(t *testing.T) {
	primeMirror(t)

	good := map[string]string{
		"https://flibusta.is/b/6013/fb2":  "https://flibusta.is/b/6013/fb2",
		"https://flibusta.net/b/6013/fb2": "https://flibusta.net/b/6013/fb2",
		"/b/6013/fb2":                     "https://flibusta.is/b/6013/fb2",
	}
	for id, want := range good {
		got, err := resolveDownloadURL(id, "fb2")
		if err != nil {
			t.Fatalf("resolveDownloadURL(%q): %v", id, err)
		}
		if got != want {
			t.Errorf("resolveDownloadURL(%q) = %q, want %q", id, got, want)
		}
	}

	// Числовой id + формат с расширением на зеркале.
	got, err := resolveDownloadURL("6013", "epub")
	if err != nil {
		t.Fatalf("resolveDownloadURL(6013, epub): %v", err)
	}
	if got != "https://flibusta.is/ebooks/6013.epub.noimages" {
		t.Errorf("URL = %q", got)
	}
	if _, err := resolveDownloadURL("6013", "fb2"); !errors.Is(err, errBadRequest) {
		t.Errorf("числовой id + fb2 без прямого URL должен отклоняться, получено: %v", err)
	}

	// Мусорные id.
	for _, id := range []string{"", "../../../etc/passwd", "abc", "6013; rm -rf /"} {
		if _, err := resolveDownloadURL(id, "epub"); !errors.Is(err, errBadRequest) {
			t.Errorf("resolveDownloadURL(%q) должен отклоняться, получено: %v", id, err)
		}
	}
}

// Локальное зеркало (FLIBUSTA_LOCAL_URL) тоже должно быть доверенным,
// иначе сломается профиль flibusta-local.
func TestTrustedDownloadHostsIncludesLocalMirror(t *testing.T) {
	t.Setenv("FLIBUSTA_LOCAL_URL", "http://host.containers.internal:27100")
	hosts := trustedDownloadHosts()
	if !hosts["host.containers.internal"] {
		t.Fatalf("локальное зеркало не в whitelist: %v", hosts)
	}
	if hosts["attacker.example"] {
		t.Fatal("чужой домен попал в whitelist")
	}
	if !hosts["flibusta.is"] || !hosts["flibusta.net"] {
		t.Fatalf("зеркала Флибусты не в whitelist: %v", hosts)
	}
}

// Регрессия «фикс не сломал фичу»: скачивание с доверенного зеркала
// должно по-прежнему работать и класть файл строго в booksDir.
func TestDownloadBookHappyPathAllowedMirror(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/b/6013/epub" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte("EPUB-CONTENT"))
	}))
	defer srv.Close()
	t.Setenv("FLIBUSTA_MIRRORS", srv.URL)
	primeMirror(t)

	dir := t.TempDir()
	prevDir := booksDir
	booksDir = dir
	t.Cleanup(func() { booksDir = prevDir })

	if err := downloadBook(srv.URL+"/b/6013/epub", "Война и мир", "epub"); err != nil {
		t.Fatalf("скачивание с доверенного зеркала упало: %v", err)
	}
	dest := filepath.Join(dir, "Война и мир.epub")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("файл не создан: %v", err)
	}
	if string(data) != "EPUB-CONTENT" {
		t.Fatalf("содержимое = %q", string(data))
	}
}

// Тот же сценарий, но зеркало НЕ в whitelist — файл не создаётся.
func TestDownloadBookRejectsUnlistedMirrorNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("SECRET"))
	}))
	defer srv.Close()
	primeMirror(t) // активное зеркало = flibusta.is, в списке зеркал srv.URL нет

	dir := t.TempDir()
	prevDir := booksDir
	booksDir = dir
	t.Cleanup(func() { booksDir = prevDir })

	err := downloadBook(srv.URL+"/b/1/epub", "Книга", "epub")
	if !errors.Is(err, errBadRequest) {
		t.Fatalf("ожидался отказ по SSRF, получено: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("файлы созданы несмотря на отказ: %v", entries)
	}
}
