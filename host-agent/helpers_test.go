package main

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilepathHelpers(t *testing.T) {
	baseCases := []struct{ in, want string }{
		{"/mnt/nas/root/FuzzyHD/Movies/Foo/bar.mkv", "bar.mkv"},
		{"plain.mp4", "plain.mp4"},
		{"/a/b/", "b"},
	}
	for _, tc := range baseCases {
		if got := filepathBase(tc.in); got != tc.want {
			t.Errorf("filepathBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	extCases := []struct{ in, want string }{
		{"/a/b/movie.2160p.mkv", ".mkv"},
		{"noext", ""},
		{".bashrc", ".bashrc"},
	}
	for _, tc := range extCases {
		if got := filepathExt(tc.in); got != tc.want {
			t.Errorf("filepathExt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if got, want := filepathJoin("/mnt/nas/Backups/duplicates", "x.mkv"), filepath.Join("/mnt/nas/Backups/duplicates", "x.mkv"); got != want {
		t.Errorf("filepathJoin = %q, want %q", got, want)
	}
}

func TestParseHumanSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1K", 1024},
		{"1M", 1 << 20},
		{"1,5G", 1610612736},
		{"1.5G", 1610612736},
		{"1.2GB", 1288490188},
		{"1 GiB", 1 << 30},
		{"300M", 314572800},
		{"12KB", 12288},
		{"1T", 1 << 40},
		{"1g", 1 << 30},
		{" 1G ", 1 << 30},
		{"0B", 0},
		{"1B", 1},
		{"", 0},
		{"abc", 0},
		{"G", 0},
		{"1X", 0},
		{"-1G", 0},
		{"Inf", 0},
		{"NaN", 0},
		{"9999999999T", math.MaxInt64},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseHumanSize(tc.in); got != tc.want {
				t.Fatalf("parseHumanSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidIface(t *testing.T) {
	good := []string{"eno1", "eth0", "wlan0", "br-abc123", "veth.1", "eth0.100", "br0@if2"}
	bad := []string{"", "-help", "--", "a b", "../x", "eth0;id", "/", strings.Repeat("a", 16)}

	for _, s := range good {
		if !validIface(s) {
			t.Errorf("validIface(%q) = false", s)
		}
	}
	for _, s := range bad {
		if validIface(s) {
			t.Errorf("validIface(%q) = true", s)
		}
	}
}
