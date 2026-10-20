package main

import "testing"

func TestParseBoundedEdges(t *testing.T) {
	cases := []struct {
		in       string
		def      int
		min, max int
		want     int
		wantErr  bool
	}{
		{"", 100, 1, 100, 100, false},
		{"   ", 100, 1, 100, 100, false},
		{"+42", 100, 1, 100, 42, false},
		{"0", 100, 0, 100, 0, false},
		{"-3", 0, 0, 10000, 0, true},
		{"101", 100, 1, 100, 0, true},
		{"abc", 100, 1, 100, 0, true},
		{"1.5", 100, 1, 100, 0, true},
		{"99999999999999999999", 100, 1, 100, 0, true},
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
