package main

import "testing"

func TestFilepathBaseExt(t *testing.T) {
	if got := filepathBase("/mnt/nas/root/FuzzyHD/Movies/Foo/bar.mkv"); got != "bar.mkv" {
		t.Errorf("filepathBase = %q", got)
	}
	if got := filepathBase("plain.mp4"); got != "plain.mp4" {
		t.Errorf("filepathBase = %q", got)
	}
	if got := filepathExt("/a/b/movie.2160p.mkv"); got != ".mkv" {
		t.Errorf("filepathExt = %q", got)
	}
	if got := filepathExt("noext"); got != "" {
		t.Errorf("filepathExt = %q, want empty", got)
	}
	if got := filepathJoin("/mnt/nas/Backups/duplicates", "x.mkv"); got != "/mnt/nas/Backups/duplicates/x.mkv" {
		t.Errorf("filepathJoin = %q", got)
	}
}

func TestParseHumanSize(t *testing.T) {
	cases := map[string]int64{
		"1K":   1024,
		"1M":   1024 * 1024,
		"1,5G": 1610612736,
		"300M": 314572800,
		"":     0,
	}
	for in, want := range cases {
		if got := parseHumanSize(in); got != want {
			t.Errorf("parseHumanSize(%q) = %d, want %d", in, got, want)
		}
	}
}
