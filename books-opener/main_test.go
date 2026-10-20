package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveMedia обязан отсекать traversal даже при совпадающем строковом
// префиксе и не выходить за пределы разрешённых корней.
func TestResolveMediaRejectsTraversal(t *testing.T) {
	for _, p := range []string{
		"/mnt/nas/YT_video/../../etc/passwd",
		"/mnt/nas/root/FuzzyHD/Movies/../../../etc/shadow",
		"/mnt/nas/YT_video/../Books/x.epub",
		"/etc/passwd",
		"/mnt/nas/YT_video/",
		"/mnt/nas/YT_video",
		"relative/path.mkv",
	} {
		if _, err := resolveMedia(p); err == nil {
			t.Errorf("resolveMedia(%q) принял путь", p)
		}
	}
}

func TestResolveUnderRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.epub")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "evil.epub")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlink not supported")
	}
	if _, err := resolveUnder(root, "evil.epub", bookExts); err == nil {
		t.Fatal("симлинк за пределы root прошёл")
	}
}

func TestResolveUnderRejectsTraversalNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"../x.epub", "a/b.epub", ".hidden.epub", "-rf.epub", "x.sh", ""} {
		if _, err := resolveUnder(root, name, bookExts); err == nil {
			t.Errorf("resolveUnder(%q) принял имя", name)
		}
	}
}
