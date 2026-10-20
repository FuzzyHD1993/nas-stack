package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0xFF, 0xD9}

func resetPhotoCacheState() {
	photoCacheLocks.Lock()
	photoCacheLocks.m = make(map[string]*photoKeyLock)
	photoCacheLocks.Unlock()

	photoFailures.Lock()
	photoFailures.m = make(map[string]time.Time)
	photoFailures.Unlock()
}

func withPhotoCache(t *testing.T) string {
	t.Helper()
	old := photoCacheDir
	dir := t.TempDir()
	photoCacheDir = dir
	resetPhotoCacheState()
	if err := initPhotoCache(); err != nil {
		t.Fatalf("initPhotoCache: %v", err)
	}
	t.Cleanup(func() {
		photoCacheDir = old
		resetPhotoCacheState()
	})
	return dir
}

func withPhotoRoot(t *testing.T) (cacheDir, root string) {
	t.Helper()
	cacheDir = withPhotoCache(t)
	root = t.TempDir()
	t.Setenv("MEDIA_PHOTOS_DIR", root)
	return cacheDir, root
}

func mustCachePath(t *testing.T, rel string, size int, kind string) string {
	t.Helper()
	p, err := photoCachePath(rel, size, kind)
	if err != nil {
		t.Fatalf("photoCachePath(%q, %d, %q): %v", rel, size, kind, err)
	}
	return p
}

func mustSave(t *testing.T, rel string, size int, kind, orig string) string {
	t.Helper()
	p, err := saveCachedPhoto(rel, size, kind, orig, testJPEG)
	if err != nil {
		t.Fatalf("saveCachedPhoto: %v", err)
	}
	return p
}

func writeJPEG(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, testJPEG, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPhotoCacheInvalidationByMtime(t *testing.T) {
	withPhotoCache(t)

	orig := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(orig, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(orig, old, old); err != nil {
		t.Fatal(err)
	}

	p := mustSave(t, "a/photo.jpg", 400, photoCacheKindThumb, orig)
	if got, ok := cachedPhoto("a/photo.jpg", 400, photoCacheKindThumb, orig); !ok || got != p {
		t.Fatal("fresh cache not found")
	}

	future := old.Add(time.Hour)
	if err := os.Chtimes(orig, future, future); err != nil {
		t.Fatal(err)
	}
	if _, ok := cachedPhoto("a/photo.jpg", 400, photoCacheKindThumb, orig); ok {
		t.Error("stale cache accepted after mtime change")
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("cachedPhoto must not delete files outside the key lock: %v", err)
	}
}

func TestPhotoCacheInvalidatedBySizeWithSameMtime(t *testing.T) {
	withPhotoCache(t)

	orig := filepath.Join(t.TempDir(), "photo.jpg")
	mt := time.Unix(1_600_000_000, 0)
	if err := os.WriteFile(orig, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orig, mt, mt); err != nil {
		t.Fatal(err)
	}
	mustSave(t, "a/photo.jpg", 400, photoCacheKindThumb, orig)

	if err := os.WriteFile(orig, []byte("version-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orig, mt, mt); err != nil {
		t.Fatal(err)
	}
	if _, ok := cachedPhoto("a/photo.jpg", 400, photoCacheKindThumb, orig); ok {
		t.Error("cache must be invalid after content change with equal mtime")
	}
}

func TestPhotoCacheCanonicalRel(t *testing.T) {
	withPhotoCache(t)

	base, err := photoCacheKey("a/photo.jpg", 400, photoCacheKindThumb)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"a/photo.jpg",
		"./a/photo.jpg",
		"a//photo.jpg",
		"a/../a/photo.jpg",
		"/a/photo.jpg",
	} {
		got, err := photoCacheKey(rel, 400, photoCacheKindThumb)
		if err != nil {
			t.Fatalf("photoCacheKey(%q): %v", rel, err)
		}
		if got != base {
			t.Errorf("rel %q: key %q != %q", rel, got, base)
		}
	}
}

func TestPhotoCacheKeysDiffer(t *testing.T) {
	dir := withPhotoCache(t)

	paths := []string{
		mustCachePath(t, "x.jpg", 400, photoCacheKindThumb),
		mustCachePath(t, "x.jpg", 800, photoCacheKindThumb),
		mustCachePath(t, "x.jpg", 1200, photoCacheKindFull),
		mustCachePath(t, "y.jpg", 400, photoCacheKindThumb),
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Fatalf("duplicate cache path: %q", p)
		}
		seen[p] = true
		r, err := filepath.Rel(dir, p)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			t.Fatalf("path outside cache dir: %q", p)
		}
	}
}

func TestPhotoCacheRejectsBadKindAndSize(t *testing.T) {
	withPhotoCache(t)

	for _, kind := range []string{"", "../../etc/x", "thumb/../../x", "Thumb", "thumb\x00"} {
		if _, err := photoCacheKey("a.jpg", 400, kind); err == nil {
			t.Errorf("kind %q must be rejected", kind)
		}
	}
	for _, size := range []int{0, 10, 63, 4097, 100000} {
		if _, err := photoCacheKey("a.jpg", size, photoCacheKindThumb); err == nil {
			t.Errorf("size %d must be rejected", size)
		}
	}
}

func TestSaveCachedPhotoRejectsNonJPEG(t *testing.T) {
	withPhotoCache(t)
	orig := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(orig, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := saveCachedPhoto("a.jpg", 400, photoCacheKindThumb, orig, []byte("not-a-jpeg")); err == nil {
		t.Fatal("non-JPEG output must be rejected")
	}
}

func TestSaveCachedPhotoLeavesNoTempFiles(t *testing.T) {
	dir := withPhotoCache(t)
	orig := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(orig, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustSave(t, "a.jpg", 400, photoCacheKindThumb, orig)

	tmp, err := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Fatalf("temp files left: %v", tmp)
	}
}

func TestClearPhotoCache(t *testing.T) {
	dir := withPhotoCache(t)

	if err := os.WriteFile(filepath.Join(dir, "x.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.jpg.tmp"), []byte("tmp"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}

	if err := clearPhotoCache(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cache not cleared: %d entries", len(entries))
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("RemoveAll followed symlink: %v", err)
	}
}

func TestClearPhotoCacheRecreatesMissingDir(t *testing.T) {
	dir := withPhotoCache(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := clearPhotoCache(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("cache dir not recreated: %v", err)
	}
}

func TestPhotoCacheLockNoLeak(t *testing.T) {
	withPhotoCache(t)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := lockPhotoCache("same-key")
			unlock()
		}()
	}
	wg.Wait()

	photoCacheLocks.Lock()
	n := len(photoCacheLocks.m)
	photoCacheLocks.Unlock()
	if n != 0 {
		t.Fatalf("photo lock map leaked %d entries", n)
	}
}

func TestValidatedOrigPath(t *testing.T) {
	_, root := withPhotoRoot(t)

	outside := filepath.Join(t.TempDir(), "outside.jpg")
	writeJPEG(t, outside)
	if _, err := validatedOrigPath(outside); err == nil {
		t.Error("path outside MEDIA_PHOTOS_DIR must be rejected")
	}

	inside := filepath.Join(root, "inside.jpg")
	writeJPEG(t, inside)
	if _, err := validatedOrigPath(inside); err != nil {
		t.Errorf("inside path rejected: %v", err)
	}

	link := filepath.Join(root, "link.jpg")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := validatedOrigPath(link); err == nil {
			t.Error("symlink escape must be rejected")
		}
	}
}
