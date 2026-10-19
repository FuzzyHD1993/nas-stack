package main

import "testing"

func TestParseIntDefault(t *testing.T) {
	cases := []struct {
		in        string
		def, want int
	}{
		{"42", 10, 42}, {"", 10, 10}, {"abc", 7, 7}, {"-3", 1, -3},
	}
	for _, c := range cases {
		if got := parseIntDefault(c.in, c.def); got != c.want {
			t.Errorf("parseIntDefault(%q,%d)=%d want %d", c.in, c.def, got, c.want)
		}
	}
}
