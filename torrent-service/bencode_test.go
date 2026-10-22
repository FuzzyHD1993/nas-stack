package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// bstr кодирует bencode-строку: "<len>:<data>".
func bstr(s string) string {
	return fmt.Sprintf("%d:%s", len(s), s)
}

func TestMagnetFromTorrent(t *testing.T) {
	trackers := []string{
		"udp://tracker.example.com:1337/announce",
		"http://backup.example.org/announce",
	}
	name := "Test Torrent (2024)"

	// Собираем .torrent вручную: info-словарь (с полями length/name/pieces),
	// затем announce/announce-list в корневом словаре. Порядок ключей bencode
	// сканер не требует, но пишем «как в файле».
	var info strings.Builder
	info.WriteString("d")
	info.WriteString("6:lengthi1000e")
	info.WriteString("4:name" + bstr(name))
	info.WriteString("6:pieces" + bstr(strings.Repeat("p", 20)))
	info.WriteString("e")
	infoRaw := info.String()

	var top strings.Builder
	top.WriteString("d")
	top.WriteString("8:announce" + bstr(trackers[0]))
	top.WriteString("13:announce-listl")
	for _, tr := range append(trackers, trackers[0]) { // дубликат в announce-list
		top.WriteString("l" + bstr(tr) + "e")
	}
	top.WriteString("e") // конец announce-list
	top.WriteString("4:info" + infoRaw)
	top.WriteString("e") // конец корневого словаря
	raw := []byte(top.String())

	// btih считаем в тесте независимо: SHA-1 от сырых байтов info-спана.
	sum := sha1.Sum([]byte(infoRaw))
	wantBTIH := hex.EncodeToString(sum[:])
	want := "magnet:?xt=urn:btih:" + wantBTIH +
		"&dn=" + url.QueryEscape(name) +
		"&tr=" + url.QueryEscape(trackers[0]) +
		"&tr=" + url.QueryEscape(trackers[1])

	got, err := magnetFromTorrent(raw)
	if err != nil {
		t.Fatalf("magnetFromTorrent: unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("magnet =\n %q\nwant\n %q", got, want)
	}
	// Дедуп: трекеров ровно два — дубль из announce-list и announce пропали.
	if n := strings.Count(got, "&tr="); n != len(trackers) {
		t.Fatalf("tracker count = %d (got %q)", n, got)
	}
}

func TestMagnetFromTorrentTrackerCap(t *testing.T) {
	var top strings.Builder
	top.WriteString("d")
	top.WriteString("13:announce-listl")
	for i := 0; i < 25; i++ {
		top.WriteString("l" + bstr(fmt.Sprintf("udp://t%d/announce", i)) + "e")
	}
	top.WriteString("e")
	top.WriteString("4:infod6:lengthi1ee") // в info достаточно любого словаря
	top.WriteString("e")
	got, err := magnetFromTorrent([]byte(top.String()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := strings.Count(got, "&tr="); n != maxMagnetTrackers {
		t.Fatalf("trackers = %d, want cap %d", n, maxMagnetTrackers)
	}
	if !strings.HasPrefix(got, "magnet:?xt=urn:btih:") {
		t.Fatalf("magnet without btih: %q", got)
	}
}

func TestMagnetFromTorrentGarbage(t *testing.T) {
	cases := map[string]string{
		"garbage":           "not a torrent at all",
		"garbage bytes":     "\x00du1ei1ee",
		"empty":             "",
		"truncated":         "d4:infod4:name",
		"only dict open":    "d",
		"unclosed list":     "l4:abc",
		"int key":           "di5e4:name5:valuee",
		"missing info":      "d8:announce28:udp://tracker.example.com/anne",
		"info not a dict":   "d4:info5:valuee",
		"absurd string len": "9999999999:a",
		"absurd int":        "i999999999999999999999999999999e",
		"trailing garbage":  "de\x00\x01\x02",
		"negative str len":  "-1:a",
		"too deeply nested": strings.Repeat("l", maxBencodeDepth+2) + strings.Repeat("e", maxBencodeDepth+2),
		"unclosed top dict": "d4:infod4:name3:abce",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := magnetFromTorrent([]byte(input))
			if err == nil {
				t.Fatalf("magnetFromTorrent(%q) = %q, want error", input, got)
			}
		})
	}
}
