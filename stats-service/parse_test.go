package main

import "testing"

func TestHumanSize(t *testing.T) {
	cases := map[uint64]string{0: "0B", 1024: "1.0K", 1536: "1.5K", 1048576: "1.0M"}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d)=%q want %q", in, got, want)
		}
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount("/mnt/nas\\040root"); got != "/mnt/nas root" {
		t.Errorf("unescapeMount = %q", got)
	}
	if got := unescapeMount("/plain"); got != "/plain" {
		t.Errorf("plain changed: %q", got)
	}
}
