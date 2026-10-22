package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	// maxBencodeDepth ограничивает вложенность синтеза, чтобы кривой .torrent
	// не уронил сервис (ни бесконечной рекурсией, ни переполнением стека).
	maxBencodeDepth = 32
	// maxMagnetTrackers — максимум трекеров в собираемом magnet.
	maxMagnetTrackers = 20
)

// errBencode — общая ошибка «это не валидный bencode».
var errBencode = errors.New("malformed bencode")

// btDict — bencode-словарь. spans хранит сырые байтовые интервалы значений
// в исходном байтовом буфере: сырой span ключа info нужен для вычисления
// btih (SHA-1 от ровно тех байтов, как они лежали в файле).
type btDict struct {
	values map[string]interface{}
	spans  map[string][2]int
}

// btDecoder — минимальный сканер bencode (без внешних зависимостей).
// Каждый шаг decode либо продвигает pos, либо возвращает ошибку:
// зациклиться или зациклить caller'а невозможно.
type btDecoder struct {
	data  []byte
	pos   int
	depth int
}

func (d *btDecoder) decode() (interface{}, error) {
	if d.depth > maxBencodeDepth {
		return nil, fmt.Errorf("%w: nesting deeper than %d", errBencode, maxBencodeDepth)
	}
	if d.pos >= len(d.data) {
		return nil, errBencode
	}
	switch c := d.data[d.pos]; {
	case c == 'i':
		return d.decodeInt()
	case c == 'l':
		return d.decodeList()
	case c == 'd':
		return d.decodeDict()
	case c >= '0' && c <= '9':
		return d.decodeString()
	default:
		return nil, errBencode
	}
}

func (d *btDecoder) decodeInt() (int64, error) {
	i := d.pos
	for i < len(d.data) && d.data[i] != 'e' {
		i++
	}
	if i >= len(d.data) || i-d.pos > 21 {
		return 0, errBencode
	}
	s := d.data[d.pos+1 : i]
	if len(s) == 0 || len(s) > 20 {
		return 0, errBencode
	}
	if s[0] == '-' {
		if len(s) == 1 {
			return 0, errBencode
		}
		s = s[1:]
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, errBencode
		}
	}
	v, err := strconv.ParseInt(string(d.data[d.pos+1:i]), 10, 64)
	if err != nil {
		return 0, errBencode
	}
	d.pos = i + 1
	return v, nil
}

func (d *btDecoder) decodeString() (string, error) {
	colon := -1
	for i := d.pos; i < len(d.data); i++ {
		ch := d.data[i]
		if ch == ':' {
			colon = i
			break
		}
		if ch < '0' || ch > '9' {
			return "", errBencode
		}
		if i-d.pos > 19 {
			return "", errBencode
		}
	}
	if colon < 0 {
		return "", errBencode
	}
	n, err := strconv.Atoi(string(d.data[d.pos:colon]))
	if err != nil {
		return "", errBencode
	}
	start := colon + 1
	if n < 0 || n > len(d.data)-start {
		return "", errBencode
	}
	d.pos = start + n
	return string(d.data[start : start+n]), nil
}

func (d *btDecoder) decodeList() ([]interface{}, error) {
	d.depth++
	defer func() { d.depth-- }()
	d.pos++ // 'l'
	out := []interface{}{}
	for {
		if d.pos >= len(d.data) {
			return nil, errBencode
		}
		if d.data[d.pos] == 'e' {
			d.pos++
			return out, nil
		}
		v, err := d.decode()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (d *btDecoder) decodeDict() (*btDict, error) {
	d.depth++
	defer func() { d.depth-- }()
	d.pos++ // 'd'
	out := &btDict{
		values: make(map[string]interface{}),
		spans:  make(map[string][2]int),
	}
	for {
		if d.pos >= len(d.data) {
			return nil, errBencode
		}
		if d.data[d.pos] == 'e' {
			d.pos++
			return out, nil
		}
		key, err := d.decodeString()
		if err != nil {
			return nil, err
		}
		if d.pos >= len(d.data) {
			return nil, errBencode
		}
		start := d.pos
		v, err := d.decode()
		if err != nil {
			return nil, err
		}
		// При дубликате ключа оставляем первое вхождение, как и большинство
		// bencode-библиотек.
		if _, dup := out.values[key]; !dup {
			out.values[key] = v
			out.spans[key] = [2]int{start, d.pos}
		}
	}
}

// magnetFromTorrent собирает magnet из .torrent-файла:
//   - btih = SHA-1 от сырых байтов значения ключа info (как в BEP-стоке);
//   - dn — из info.name (UTF-8), трекеры — announce + announce-list,
//     дедуп, максимум maxMagnetTrackers;
//   - на кривом bencode (включая мусор/обрезанные данные) — ошибка, без паники.
func magnetFromTorrent(data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("empty torrent data")
	}
	if len(data) > maxTorrentBodyBytes {
		return "", fmt.Errorf("torrent data too large: %d bytes", len(data))
	}
	dec := &btDecoder{data: data}
	v, err := dec.decode()
	if err != nil {
		return "", fmt.Errorf("malformed bencode: %w", err)
	}
	top, ok := v.(*btDict)
	if !ok {
		return "", errors.New("torrent: top-level value is not a dict")
	}
	info, ok := top.values["info"].(*btDict)
	if !ok {
		return "", errors.New("torrent: no info dict")
	}
	span, ok := top.spans["info"]
	if !ok {
		return "", errors.New("torrent: no info span")
	}
	sum := sha1.Sum(data[span[0]:span[1]])
	magnet := "magnet:?xt=urn:btih:" + hex.EncodeToString(sum[:])

	if name, ok := info.values["name"].(string); ok && name != "" {
		magnet += "&dn=" + url.QueryEscape(name)
	}

	seen := make(map[string]struct{}, maxMagnetTrackers)
	trackers := make([]string, 0, maxMagnetTrackers)
	addTracker := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || len(trackers) >= maxMagnetTrackers {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		trackers = append(trackers, s)
	}
	if ann, ok := top.values["announce"].(string); ok {
		addTracker(ann)
	}
	if al, ok := top.values["announce-list"].([]interface{}); ok {
		for _, item := range al {
			lst, ok := item.([]interface{})
			if !ok {
				continue
			}
			for _, e := range lst {
				s, ok := e.(string)
				if ok {
					addTracker(s)
				}
			}
		}
	}
	for _, tr := range trackers {
		magnet += "&tr=" + url.QueryEscape(tr)
	}
	return magnet, nil
}
