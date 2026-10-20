package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

var (
	rdb *redis.Client
	db  *pgxpool.Pool
	ctx = context.Background()
)

// bitPerfectEnabled — показывать ли bit-perfect индикатор (для EasyEffects/EQ выключают).
// atomic: читается из HTTP-хендлеров, playTracker и тестов.
var bitPerfectEnabled atomic.Bool

func init() { bitPerfectEnabled.Store(true) }

// mpdClient — минимальный клиент MPD поверх TCP.
type mpdClient struct {
	conn   net.Conn
	mu     sync.Mutex
	r      *bufio.Reader
	closed atomic.Bool
}

// close идемпотентен и не требует mu: он «будит» активный I/O.
func (c *mpdClient) close() {
	if c.closed.Swap(true) {
		return
	}
	_ = c.conn.SetDeadline(time.Now())
	_ = c.conn.Close()
}

func dialMPD(addr string) (*mpdClient, error) {
	var conn net.Conn
	var err error
	for i := 0; i < 3; i++ {
		conn, err = net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	c := &mpdClient{conn: conn, r: bufio.NewReader(conn)}
	if _, err := c.rawTimeout(3*time.Second, "ping"); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func mpdQuote(s string) string {
	// MPD читает команды построчно: \n/\r — это инъекция новой команды.
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == 0 {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"\\") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// defaultMPDTimeout — верхняя граница любой команды MPD: зависший MPD не
// должен навсегда удерживать c.mu (и все последующие запросы).
const defaultMPDTimeout = 15 * time.Second

func (c *mpdClient) raw(cmd string, args ...string) ([]string, error) {
	return c.rawTimeout(defaultMPDTimeout, cmd, args...)
}

// rawTimeout — raw с дедлайном на весь обмен. При ошибке соединение
// закрывается: иначе в bufio.Reader остаётся недочитанный хвост ответа, и
// следующая команда читает чужие строки (десинхронизация протокола MPD).
func (c *mpdClient) rawTimeout(timeout time.Duration, cmd string, args ...string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed.Load() {
		return nil, fmt.Errorf("mpd: connection closed")
	}
	if err := c.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		c.close()
		return nil, err
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	return c.exchange(cmd, args...)
}

func (c *mpdClient) exchange(cmd string, args ...string) ([]string, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("mpd: connection closed")
	}
	line := cmd
	if len(args) > 0 {
		quoted := make([]string, 0, len(args))
		for _, a := range args {
			quoted = append(quoted, mpdQuote(a))
		}
		line += " " + strings.Join(quoted, " ")
	}

	if _, err := fmt.Fprintf(c.conn, "%s\n", line); err != nil {
		c.close()
		return nil, err
	}
	var out []string
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			c.close()
			return nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "OK" {
			return out, nil
		}
		if strings.HasPrefix(l, "ACK") {
			return nil, fmt.Errorf("mpd: %s", l)
		}
		out = append(out, l)
	}
}

func (c *mpdClient) exec(cmd string, args ...string) error {
	_, err := c.raw(cmd, args...)
	return err
}

type mpdField struct {
	key string
	val string
}

func (c *mpdClient) fields(cmd string, args ...string) []mpdField {
	lines, _ := c.raw(cmd, args...)
	fs := []mpdField{}
	for _, l := range lines {
		if i := strings.Index(l, ": "); i >= 0 {
			fs = append(fs, mpdField{key: l[:i], val: l[i+2:]})
		}
	}
	return fs
}

func fieldsToMap(fs []mpdField) fiber.Map {
	m := fiber.Map{}
	for _, f := range fs {
		// MPD возвращает теги с большой буквы (Artist/Album/Title) и служебные
		// поля строчными (file/Time) — приводим ключи к нижнему регистру.
		m[strings.ToLower(f.key)] = f.val
	}
	return m
}

func parseTrackList(lines []string) []fiber.Map {
	tracks := []fiber.Map{}
	var cur fiber.Map
	flush := func() {
		if cur != nil {
			tracks = append(tracks, cur)
		}
		cur = nil
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "file: ") {
			flush()
			cur = fiber.Map{"file": strings.TrimPrefix(l, "file: ")}
			continue
		}
		if cur == nil {
			continue
		}
		if i := strings.Index(l, ": "); i >= 0 {
			k := strings.ToLower(l[:i])
			v := l[i+2:]
			switch k {
			case "title", "artist", "album", "pos", "id", "track":
				cur[k] = v
			case "duration":
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					cur["duration"] = n
				}
			}
		}
	}
	flush()
	return tracks
}

// ——————————————————————————————
// Управляемый клиент (глобальный)
// ——————————————————————————————

var (
	mpdMu      sync.Mutex
	mpd        *mpdClient
	dialMu     sync.Mutex // сериализует переподключение (один конкурентный диал)
	mpdDownFor time.Time  // backoff после неудачного диала, защита от thundering herd (под dialMu)
)

// pingTimeout проверяет живость соединения с жёстким дедлайном.
// Не требует mpdMu; сериализуется собственным mu клиента.
func (c *mpdClient) pingTimeout(d time.Duration) bool {
	if c.closed.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetDeadline(time.Now().Add(d))
	defer c.conn.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(c.conn, "ping\n"); err != nil {
		return false
	}
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			return false
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "OK" {
			return true
		}
		if strings.HasPrefix(l, "ACK") {
			return false
		}
	}
}

// mpdReachable — быстрый healthcheck без ретраев и глобального состояния:
// один dial до 1 с + ping через mpdClient.raw (баннер "OK MPD …" пропускается,
// ответ ждём до "OK"/"ACK").
func mpdReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(1 * time.Second))
	c := &mpdClient{conn: conn, r: bufio.NewReader(conn)}
	_, err = c.rawTimeout(1*time.Second, "ping")
	return err == nil
}

// getMPD возвращает живое соединение с MPD. Инвариант: сетевые операции
// (ping, dial) выполняются ВНЕ mpdMu — глобальный лок удерживается только на
// чтение/подмену указателя, поэтому медленный MPD не блокирует остальные хендлеры.
func getMPD() (*mpdClient, error) {
	mpdMu.Lock()
	c := mpd
	mpdMu.Unlock()
	if c != nil && !c.closed.Load() && c.pingTimeout(2*time.Second) {
		return c, nil
	}
	dialMu.Lock()
	defer dialMu.Unlock()
	// Кто-то из конкурентов мог уже переподключиться, пока ждали dialMu.
	mpdMu.Lock()
	c = mpd
	mpdMu.Unlock()
	if c != nil && !c.closed.Load() && c.pingTimeout(2*time.Second) {
		return c, nil
	}
	if time.Now().Before(mpdDownFor) {
		return nil, fmt.Errorf("mpd: temporarily unavailable (backoff)")
	}
	if c != nil {
		mpdMu.Lock()
		if mpd == c {
			mpd = nil
		}
		mpdMu.Unlock()
		c.close()
	}
	nc, err := dialMPD(platform.GetEnv("MPD_ADDR", "host.containers.internal:6600"))
	if err != nil {
		mpdDownFor = time.Now().Add(2 * time.Second)
		return nil, err
	}
	mpdMu.Lock()
	mpd = nc
	mpdMu.Unlock()
	return nc, nil
}

// ——————————————————————————————
// Бизнес-функции
// ——————————————————————————————

func mpdStatus() fiber.Map {
	c, err := getMPD()
	if err != nil {
		return fiber.Map{"state": "stop", "mpd": "down"}
	}
	fs := c.fields("status")
	st := fiber.Map{}
	for _, f := range fs {
		switch f.key {
		case "repeat", "random", "single", "consume":
			st[f.key] = f.val == "1"
		case "elapsed", "duration":
			if v, err := strconv.ParseFloat(f.val, 64); err == nil {
				st[f.key] = v
			}
		case "volume", "playlistlength", "song":
			if v, err := strconv.Atoi(f.val); err == nil {
				st[f.key] = v
			} else {
				st[f.key] = f.val
			}
		default:
			st[f.key] = f.val
		}
	}
	si := fieldsToMap(c.fields("currentsong"))
	st["song_info"] = si
	// аудио-метаданные текущего трека (FLAC 24bit/96kHz, DSD64...)
	songFile := ""
	if v, ok := si["file"].(string); ok {
		songFile = v
	}
	raw := map[string]string{}
	for _, f := range fs {
		raw[f.key] = f.val
	}
	st["audio"] = parseAudioInfo(songFile, raw)
	st["audio_chain_enabled"] = bitPerfectEnabled.Load()
	st["mpd"] = "connected"
	return st
}

type AudioInfo struct {
	SampleRate   int    `json:"sample_rate"`
	Bits         int    `json:"bits"`
	Channels     int    `json:"channels"`
	Codec        string `json:"codec"`
	BitrateKbps  int    `json:"bitrate_kbps"`
	Label        string `json:"label"`
	BitPerfect   bool   `json:"bit_perfect"`
	Resampled    bool   `json:"resampled"`
	DSD          bool   `json:"dsd"`
	OutputRate   int    `json:"output_rate"`
	OutputBits   int    `json:"output_bits"`
	SourceRate   int    `json:"source_rate"`
	SourceBits   int    `json:"source_bits"`
	OutputDevice string `json:"output_device"`
}

type probeResult struct {
	rate, bits int
	codec      string
	exp        time.Time
}

var (
	probeMu    sync.RWMutex
	probeCache = map[string]probeResult{}
)

const probeTTL = 10 * time.Minute

// probeSourceAudio читает частоту/битность исходного файла через ffprobe.
// Путь приходит из MPD, поэтому нормализуется внутри MUSIC_DIR: ffprobe не
// получает URL, опцию или путь вне библиотеки. Результат кэшируется: /status
// опрашивается часто, а запуск процесса на каждый запрос недопустим.
func probeSourceAudio(songFile string) (rate, bits int, codec string) {
	if songFile == "" {
		return 0, 0, ""
	}
	rel, ok := relMusicPath(songFile)
	if !ok {
		return 0, 0, ""
	}
	p, ok := safeMusicFile(rel)
	if !ok {
		return 0, 0, ""
	}

	key := p
	if fi, err := os.Stat(p); err == nil {
		key = fmt.Sprintf("%s|%d|%d", p, fi.Size(), fi.ModTime().UnixNano())
	}
	probeMu.RLock()
	pr, ok := probeCache[key]
	probeMu.RUnlock()
	if ok && time.Now().Before(pr.exp) {
		return pr.rate, pr.bits, pr.codec
	}

	rate, bits, codec = runFFprobe(p)
	probeMu.Lock()
	if len(probeCache) > 4096 {
		probeCache = map[string]probeResult{}
	}
	probeCache[key] = probeResult{rate: rate, bits: bits, codec: codec, exp: time.Now().Add(probeTTL)}
	probeMu.Unlock()
	return rate, bits, codec
}

func runFFprobe(p string) (rate, bits int, codec string) {
	cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ffprobe",
		"-v", "error",
		"-protocol_whitelist", "file", // только локальные файлы: без http/tcp/concat
		"-select_streams", "a:0",
		"-show_entries", "stream=sample_rate,bits_per_raw_sample,bits_per_sample,codec_name",
		"-of", "default=noprint_wrappers=1",
		"file:"+p).Output()
	if err != nil {
		return 0, 0, ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		kv := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "sample_rate":
			rate, _ = strconv.Atoi(kv[1])
		case "bits_per_raw_sample", "bits_per_sample":
			if b, _ := strconv.Atoi(kv[1]); b > bits {
				bits = b
			}
		case "codec_name":
			codec = kv[1]
		}
	}
	return rate, bits, codec
}

func parseAudioInfo(songFile string, status map[string]string) AudioInfo {
	info := AudioInfo{}
	if audioStr, ok := status["audio"]; ok {
		parts := strings.Split(audioStr, ":")
		if len(parts) >= 3 {
			info.SampleRate, _ = strconv.Atoi(parts[0])
			info.Bits, _ = strconv.Atoi(parts[1])
			info.Channels, _ = strconv.Atoi(parts[2])
		}
	}
	// MPD — внешний процесс: абсурдные значения не должны попадать в расчёты.
	if info.SampleRate < 0 || info.SampleRate > 4_000_000 {
		info.SampleRate = 0
	}
	if info.Bits < 0 || info.Bits > 64 {
		info.Bits = 0
	}
	if info.Channels < 0 || info.Channels > 64 {
		info.Channels = 0
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(songFile), "."))
	if ext != "" {
		info.Codec = ext
	}
	if br, ok := status["bitrate"]; ok {
		info.BitrateKbps, _ = strconv.Atoi(br)
	} else if info.SampleRate > 0 && info.Bits > 0 {
		info.BitrateKbps = info.SampleRate * info.Bits * info.Channels / 1000
	}
	if info.BitrateKbps < 0 || info.BitrateKbps > 100_000 {
		info.BitrateKbps = 0
	}
	codecUpper := strings.ToUpper(info.Codec)
	switch {
	case info.Codec == "dsf" || info.Codec == "dff":
		switch {
		case info.SampleRate >= 5644800:
			info.Label = "DSD128"
		case info.SampleRate >= 2822400:
			info.Label = "DSD64"
		default:
			info.Label = "DSD"
		}
	case info.SampleRate > 0 && info.Bits > 0:
		if codecUpper != "" {
			info.Label = fmt.Sprintf("%s %dbit/%.1fkHz", codecUpper, info.Bits, float64(info.SampleRate)/1000.0)
		} else {
			info.Label = fmt.Sprintf("%dbit/%.1fkHz", info.Bits, float64(info.SampleRate)/1000.0)
		}
	case info.BitrateKbps > 0:
		info.Label = fmt.Sprintf("%s %dkbps", codecUpper, info.BitrateKbps)
	default:
		info.Label = codecUpper
	}

	// Если индикатор выключен — не вызываем ffprobe и не считаем bit-perfect.
	if !bitPerfectEnabled.Load() {
		return info
	}

	// Выход MPD (формат аудиовыхода из status.audio)
	info.OutputRate = info.SampleRate
	info.OutputBits = info.Bits
	info.OutputDevice = "MPD output"

	// Источник (через ffprobe, если доступен)
	srcRate, srcBits, srcCodec := probeSourceAudio(songFile)
	if srcRate == 0 {
		srcRate = info.SampleRate
	}
	if srcBits == 0 {
		srcBits = info.Bits
	}
	info.SourceRate = srcRate
	info.SourceBits = srcBits

	info.DSD = info.Codec == "dsf" || info.Codec == "dff" ||
		strings.Contains(strings.ToLower(srcCodec), "dsd") || info.SampleRate > 1000000

	if !info.DSD && srcRate > 0 && info.SampleRate > 0 {
		if srcRate != info.SampleRate {
			info.Resampled = true
		} else if srcBits == 0 || info.Bits == 0 || srcBits == info.Bits {
			info.BitPerfect = true
		}
	}
	return info
}

func albumList() ([]fiber.Map, error) {
	c, err := getMPD()
	if err != nil {
		return nil, err
	}
	lines, err := c.rawTimeout(60*time.Second, "list", "Album", "group", "AlbumArtist")
	if err != nil {
		return nil, err
	}
	var cur string
	albums := []fiber.Map{} // пустой, но валидный список — не ошибка
	for _, l := range lines {
		if strings.HasPrefix(l, "AlbumArtist: ") {
			cur = strings.TrimPrefix(l, "AlbumArtist: ")
		} else if strings.HasPrefix(l, "Album: ") {
			alb := strings.TrimPrefix(l, "Album: ")
			if alb == "" || cur == "" {
				continue
			}
			albums = append(albums, fiber.Map{
				"artist":    cur,
				"album":     alb,
				"cover_url": "/api/music/covers/" + url.PathEscape(cur) + "/" + url.PathEscape(alb),
			})
		}
	}
	sort.Slice(albums, func(i, j int) bool {
		a := strings.ToLower(albums[i]["artist"].(string))
		b := strings.ToLower(albums[j]["artist"].(string))
		if a != b {
			return a < b
		}
		return strings.ToLower(albums[i]["album"].(string)) < strings.ToLower(albums[j]["album"].(string))
	})
	return albums, nil
}

// TTL кэша в Redis: список альбомов — 5 минут, треки альбома — 10 минут.
const (
	albumsCacheTTL = 5 * time.Minute
	tracksCacheTTL = 10 * time.Minute
)

// normalizeAlbumSort приводит параметр сортировки к одному из поддерживаемых
// значений; неизвестное значение → "artist".
func normalizeAlbumSort(sortParam string) string {
	switch sortParam {
	case "album", "recent", "year_desc", "year_asc":
		return sortParam
	default:
		return "artist"
	}
}

// cacheGetJSON читает JSON из Redis (fail-open: ошибка/промах → false).
func cacheGetJSON(key string, dst any) bool {
	if rdb == nil {
		return false
	}
	b, err := rdb.Get(ctx, key).Bytes()
	if err != nil {
		return false
	}
	return json.Unmarshal(b, dst) == nil
}

// cacheSetJSON кладёт JSON в Redis (fail-open: ошибки игнорируются).
func cacheSetJSON(key string, v any, ttl time.Duration) {
	if rdb == nil {
		return
	}
	if b, err := json.Marshal(v); err == nil {
		_ = rdb.Set(ctx, key, b, ttl).Err()
	}
}

func getAlbumsSorted(sortParam string) ([]fiber.Map, error) {
	sortID := normalizeAlbumSort(sortParam)
	key := "music:albums:" + sortID
	var cached []fiber.Map
	if cacheGetJSON(key, &cached) {
		return cached, nil
	}
	albums, err := albumList()
	if err != nil {
		return nil, err
	}
	switch sortID {
	case "album":
		sort.Slice(albums, func(i, j int) bool {
			ai := strings.ToLower(albums[i]["album"].(string))
			aj := strings.ToLower(albums[j]["album"].(string))
			if ai != aj {
				return ai < aj
			}
			return strings.ToLower(albums[i]["artist"].(string)) < strings.ToLower(albums[j]["artist"].(string))
		})
	case "recent", "year_desc", "year_asc", "artist":
		// сортировка по году/mtime требует доп. данных — fallback на artist
		// (albumList уже отсортирован по artist).
	}
	cacheSetJSON(key, albums, albumsCacheTTL)
	return albums, nil
}

// trackNo безопасно достаёт номер трека: тега может не быть вовсе.
func trackNo(m fiber.Map) int {
	switch v := m["track"].(type) {
	case string:
		if i := strings.IndexByte(v, '/'); i > 0 {
			v = v[:i]
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return 0
		}
		return n
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0
		}
		return int(v)
	case int:
		if v < 0 {
			return 0
		}
		return v
	}
	return 0
}

func albumTracks(artist, album string) ([]fiber.Map, error) {
	key := "music:tracks:" + url.QueryEscape(artist) + ":" + url.QueryEscape(album)
	var cached []fiber.Map
	if cacheGetJSON(key, &cached) {
		return cached, nil
	}
	c, err := getMPD()
	if err != nil {
		return nil, err
	}
	lines, err := c.raw("find", "Album", album, "AlbumArtist", artist)
	if err != nil {
		return nil, err
	}
	// Fallback без артиста только если артист не задан: иначе можно вернуть
	// и закэшировать треки одноимённого альбома другого артиста.
	if len(lines) == 0 && strings.TrimSpace(artist) == "" {
		lines, err = c.raw("find", "Album", album)
		if err != nil {
			return nil, err
		}
	}
	tracks := []fiber.Map{}
	var cur fiber.Map
	flush := func() {
		if cur != nil {
			tracks = append(tracks, cur)
		}
		cur = nil
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "file: ") {
			flush()
			cur = fiber.Map{"file": strings.TrimPrefix(l, "file: ")}
			continue
		}
		if cur == nil {
			continue
		}
		if i := strings.Index(l, ": "); i >= 0 {
			k := strings.ToLower(l[:i])
			v := l[i+2:]
			switch k {
			case "title":
				cur["title"] = v
			case "artist":
				cur["artist"] = v
			case "track":
				cur["track"] = v
			case "duration":
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					cur["duration"] = n
				}
			}
		}
	}
	flush()
	sort.SliceStable(tracks, func(i, j int) bool {
		return trackNo(tracks[i]) < trackNo(tracks[j])
	})
	cacheSetJSON(key, tracks, tracksCacheTTL)
	return tracks, nil
}

func playAlbum(artist, album string) error {
	artist, album = strings.TrimSpace(artist), strings.TrimSpace(album)
	if artist == "" || album == "" {
		return fmt.Errorf("artist and album required")
	}
	m, err := getMPD()
	if err != nil {
		return err
	}
	tracks, err := albumTracks(artist, album)
	if err != nil {
		return err
	}
	files := make([]string, 0, len(tracks))
	for _, t := range tracks {
		if f, ok := t["file"].(string); ok && f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("album not found")
	}
	_, err = replaceQueueAndPlay(m, files)
	return err
}

func coverFile(artist, album string) string {
	// Полный поиск: файл в папке альбома (30+ имён + рекурсия) → встроенная
	// обложка через ffmpeg. Реализация в covers.go.
	return coverFor(artist, album)
}

// coverParam декодирует сегмент пути. PathUnescape, а не QueryUnescape:
// '+' в названии не должен превращаться в пробел.
func coverParam(c fiber.Ctx, key string) (string, bool) {
	s, err := url.PathUnescape(c.Params(key))
	if err != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 256 {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return s, true
}

// validPlaylistName — MPD пишет плейлист в playlist_directory на хосте,
// поэтому разделители пути и управляющие символы недопустимы.
func validPlaylistName(s string) bool {
	if s == "" || len([]rune(s)) > 128 || s == "." || s == ".." ||
		strings.ContainsAny(s, "/\\\r\n\x00") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func mpdSearch(q string) []fiber.Map {
	c, err := getMPD()
	if err != nil {
		return []fiber.Map{}
	}
	lines, _ := c.raw("search", "any", q)
	res := []fiber.Map{}
	var cur fiber.Map
	flush := func() {
		if cur != nil {
			res = append(res, cur)
		}
		cur = nil
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "file: ") {
			flush()
			cur = fiber.Map{"file": strings.TrimPrefix(l, "file: ")}
			continue
		}
		if cur == nil {
			continue
		}
		if i := strings.Index(l, ": "); i >= 0 {
			k := strings.ToLower(l[:i])
			v := l[i+2:]
			switch k {
			case "title", "artist", "album":
				cur[k] = v
			case "duration":
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					cur["duration"] = n
				}
			}
		}
	}
	flush()
	return res
}

var playStats = struct {
	sync.Mutex
	artists map[string]int
	tracks  map[string]int
}{artists: map[string]int{}, tracks: map[string]int{}}

// maxPlayStats ограничивает кардинальность: интернет-радио и потоковые теги
// иначе растят map без предела до OOM.
const maxPlayStats = 20000

// playTracker отслеживает смену трека и ведёт статистику прослушивания.
func playTracker() {
	last := ""
	var lastID int64
	var lastElapsed, lastDuration float64
	lastState := ""
	prevFile := ""
	var prevElapsed, prevDuration float64
	for {
		m, err := getMPD()
		if err != nil {
			// MPD недоступен — это не «альбом доиграл». Сброс обязателен,
			// иначе после реконнекта возможен ложный автопереход и clear очереди.
			lastState, prevFile = "", ""
			prevElapsed, prevDuration = 0, 0
			time.Sleep(10 * time.Second)
			continue
		}
		st := fieldsToMap(m.fields("status"))
		cur := fieldsToMap(m.fields("currentsong"))
		artist, _ := cur["artist"].(string)
		title, _ := cur["title"].(string)
		file, _ := cur["file"].(string)
		alb, _ := cur["album"].(string)
		duration, _ := strconv.ParseFloat(fmt.Sprint(st["duration"]), 64)
		elapsed, _ := strconv.ParseFloat(fmt.Sprint(st["elapsed"]), 64)
		state, _ := st["state"].(string)
		// Автопереход: альбом доиграл (play → stop у самого конца трека).
		// Ручной /stop и repeat/single не считаются концом альбома.
		if lastState == "play" && state == "stop" && prevFile != "" &&
			prevDuration > 0 && prevElapsed >= prevDuration-2 &&
			st["repeat"] != "1" && st["single"] != "1" && autoNextAllowed() {
			autoNextAlbum(m, prevFile)
		}
		key := file
		if key == "" {
			last = ""
		}
		if key != "" && key != last {
			// Завершаем предыдущую запись, если отыграла >=90%. Нулевой id
			// (дубль/служебный файл) не должен затирать lastID.
			if id := recordListen(file, artist, alb, title, duration, lastID, lastElapsed, lastDuration); id > 0 {
				lastID = id
			}
			last = key
			playStats.Lock()
			if len(playStats.tracks) < maxPlayStats && len(playStats.artists) < maxPlayStats {
				if title != "" {
					playStats.tracks[artist+" — "+title]++
				}
				if artist != "" {
					playStats.artists[artist]++
				}
			}
			playStats.Unlock()
			if rdb != nil {
				artist = strings.ToValidUTF8(artist, "?")
				title = strings.ToValidUTF8(title, "?")
				alb = strings.ToValidUTF8(alb, "?")
				payload, _ := json.Marshal(fiber.Map{"artist": artist, "title": title, "album": alb, "file": file})
				_ = rdb.Publish(ctx, "music.played", string(payload)).Err()
			}
		}
		lastElapsed, lastDuration = elapsed, duration
		lastState = state
		prevFile = file
		prevElapsed, prevDuration = elapsed, duration
		time.Sleep(10 * time.Second)
	}
}

func topN(m map[string]int, n int) []fiber.Map {
	type kv struct {
		k string
		v int
	}
	arr := make([]kv, 0, len(m))
	for k, v := range m {
		arr = append(arr, kv{k, v})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
	out := []fiber.Map{}
	for i, e := range arr {
		if i >= n {
			break
		}
		out = append(out, fiber.Map{"name": e.k, "count": e.v})
	}
	return out
}

// lyricsHTTP — общий клиент: keep-alive вместо Transport на каждый запрос.
var lyricsHTTP = &http.Client{Timeout: 10 * time.Second}

const maxLyricsBody = 512 << 10

func lyricsCacheKey(artist, track string) string {
	sum := sha256.Sum256([]byte(artist + "\x00" + track))
	return "lyrics:v2:" + hex.EncodeToString(sum[:])
}

// progressKey хэширует путь: произвольный file_path не должен создавать
// неограниченное число ключей в Redis.
func progressKey(fp string) string {
	sum := sha256.Sum256([]byte(fp))
	return "music:progress:" + hex.EncodeToString(sum[:])
}

func main() {
	if err := platform.ValidateEnv([]string{"POSTGRES_URL"}); err != nil {
		log.Fatalf("❌ %v", err)
	}
	rdb = redis.NewClient(&redis.Options{
		Addr:         platform.GetEnv("REDIS_ADDR", "localhost:6379"),
		DialTimeout:  1 * time.Second,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: 1 * time.Second,
		MaxRetries:   1,
		// v9.23.0: dialConn имеет собственный цикл ретраев диала с дефолтом
		// DialerRetries=5 (6 попыток): без явного лимита одна Redis-операция
		// при недоступном Redis висит ~13 c вместо гарантии «~1 c на попытку».
		DialerRetries: 1,
	})
	bitPerfectEnabled.Store(platform.GetEnv("BIT_PERFECT_ENABLED", "true") == "true")
	if pgURL := platform.GetEnv("POSTGRES_URL", ""); pgURL != "" {
		if p, err := pgxpool.New(ctx, pgURL); err != nil {
			log.Printf("⚠️ PostgreSQL: %v (история прослушиваний недоступна)", err)
		} else {
			db = p
			if err := migrateListens(); err != nil {
				log.Printf("⚠️ Миграция listens: %v", err)
			}
		}
	}
	startAudioModeWatcher()
	go playTracker()
	// Прогрев кэшей (скан музыки и индекс артистов) в фоне, чтобы первый
	// запрос /playlists/smart и /artists не ждал десятки секунд.
	go func() {
		time.Sleep(3 * time.Second)
		musicIndex()
		getArtistIndex()
	}()
	app := fiber.New(fiber.Config{
		ReadTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second,
		BodyLimit:   1 << 20,
	})
	platform.StartPprof()
	// metrics СНАРУЖИ recover: recover превращает панику в error, и только
	// тогда metricsMiddleware видит 500, а не 200.
	app.Use(metricsMiddleware())
	app.Use(fiberrecover.New())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error {
		state := "down"
		if mpdReachable(platform.GetEnv("MPD_ADDR", "host.containers.internal:6600")) {
			state = "connected"
		}
		return c.JSON(platform.HealthJSON("music", fiber.Map{"mpd": state}))
	})

	listensRoutes(app)
	artistsRoutes(app)
	browserRoutes(app)

	app.Get("/status", func(c fiber.Ctx) error {
		st := mpdStatus()
		st["mode"] = currentAudioMode()
		return c.JSON(st)
	})

	// Цепочка аудио: источник → выход (bit-perfect индикатор)
	app.Get("/audio/chain", func(c fiber.Ctx) error {
		if !bitPerfectEnabled.Load() {
			return c.JSON(fiber.Map{"enabled": false})
		}
		st := mpdStatus()
		ai, _ := st["audio"].(AudioInfo)
		song, _ := st["song_info"].(fiber.Map)
		return c.JSON(fiber.Map{
			"source":      fiber.Map{"codec": ai.Codec, "sample_rate": ai.SourceRate, "bits": ai.SourceBits},
			"output":      fiber.Map{"device": ai.OutputDevice, "sample_rate": ai.OutputRate, "bits": ai.OutputBits},
			"bit_perfect": ai.BitPerfect,
			"resampled":   ai.Resampled,
			"dsd":         ai.DSD,
			"label":       ai.Label,
			"song":        song,
		})
	})

	app.Get("/stats", func(c fiber.Ctx) error {
		playStats.Lock()
		defer playStats.Unlock()
		return c.JSON(fiber.Map{
			"artists": topN(playStats.artists, 10),
			"tracks":  topN(playStats.tracks, 10),
			"total":   len(playStats.tracks),
		})
	})

	app.Post("/play", func(c fiber.Ctx) error { return mpdOK(c, "play") })
	app.Post("/pause", func(c fiber.Ctx) error { return mpdOK(c, "pause") })
	app.Post("/stop", func(c fiber.Ctx) error { markManualStop(); return mpdOK(c, "stop") })
	app.Post("/next", func(c fiber.Ctx) error {
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"status": "error", "error": "mpd unavailable"})
		}
		// Если следующий трек отсутствует (одиночный файл в очереди) —
		// догружаем остальные треки его папки, чтобы next не уходил в stop.
		ensureNextTrack(m)
		_ = m.exec("next")
		if state, _ := mpdStatus()["state"].(string); state != "play" {
			_ = m.exec("play")
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/prev", func(c fiber.Ctx) error { return mpdOK(c, "previous") })
	app.Post("/clear", func(c fiber.Ctx) error { return mpdOK(c, "clear") })
	app.Post("/shuffle", func(c fiber.Ctx) error { return mpdOK(c, "shuffle") })

	app.Post("/volume", func(c fiber.Ctx) error {
		var b struct {
			Level int `json:"level"`
		}
		if err := c.Bind().JSON(&b); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		if b.Level < 0 || b.Level > 100 {
			return c.Status(400).JSON(fiber.Map{"error": "level must be 0..100"})
		}
		return mpdOK(c, "setvol", strconv.Itoa(b.Level))
	})

	app.Post("/seek", func(c fiber.Ctx) error {
		var b struct {
			Position float64 `json:"position"`
		}
		if err := c.Bind().JSON(&b); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		if math.IsNaN(b.Position) || math.IsInf(b.Position, 0) ||
			b.Position < 0 || b.Position > 24*3600 {
			return c.Status(400).JSON(fiber.Map{"error": "invalid position"})
		}
		return mpdOK(c, "seekcur", strconv.FormatFloat(b.Position, 'f', 1, 64))
	})

	app.Post("/repeat", func(c fiber.Ctx) error {
		var b struct {
			Enabled bool `json:"enabled"`
		}
		if err := c.Bind().JSON(&b); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		v := "0"
		if b.Enabled {
			v = "1"
		}
		return mpdOK(c, "repeat", v)
	})

	app.Post("/random", func(c fiber.Ctx) error {
		var b struct {
			Enabled bool `json:"enabled"`
		}
		if err := c.Bind().JSON(&b); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		v := "0"
		if b.Enabled {
			v = "1"
		}
		return mpdOK(c, "random", v)
	})

	app.Get("/playlists", func(c fiber.Ctx) error {
		m, err := getMPD()
		if err != nil {
			return c.JSON(fiber.Map{"playlists": []fiber.Map{}})
		}
		lines, _ := m.raw("listplaylists")
		res := []fiber.Map{}
		for _, l := range lines {
			if strings.HasPrefix(l, "playlist: ") {
				res = append(res, fiber.Map{"name": strings.TrimPrefix(l, "playlist: ")})
			}
		}
		return c.JSON(fiber.Map{"playlists": res})
	})

	app.Post("/playlist/:name/play", func(c fiber.Ctx) error {
		name, err := url.PathUnescape(c.Params("name"))
		if err != nil || !validPlaylistName(name) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid playlist name"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		queueMu.Lock()
		defer queueMu.Unlock()
		if err := m.exec("clear"); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "queue update failed"})
		}
		if err := m.exec("load", name); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "queue update failed"})
		}
		if err := m.exec("play"); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "play failed"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	// Очередь
	app.Get("/queue", func(c fiber.Ctx) error {
		m, err := getMPD()
		if err != nil {
			return c.JSON(fiber.Map{"tracks": []fiber.Map{}})
		}
		lines, _ := m.raw("playlistinfo")
		return c.JSON(fiber.Map{"tracks": parseTrackList(lines)})
	})
	app.Post("/queue/add", func(c fiber.Ctx) error {
		var b struct {
			File string `json:"file"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.File == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file required"})
		}
		rel, ok := relMusicPath(b.File)
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid file path"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		if err := m.exec("add", rel); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "add failed"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/queue/clear", func(c fiber.Ctx) error {
		return mpdOK(c, "clear")
	})
	app.Delete("/queue/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil || id < 0 {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		return mpdOK(c, "deleteid", strconv.Itoa(id))
	})
	app.Post("/queue/play/:id", func(c fiber.Ctx) error {
		id, err := strconv.Atoi(c.Params("id"))
		if err != nil || id < 0 {
			return c.Status(400).JSON(fiber.Map{"error": "invalid id"})
		}
		return mpdOK(c, "playid", strconv.Itoa(id))
	})

	// Сохранённые плейлисты
	app.Get("/saved-playlists", func(c fiber.Ctx) error {
		m, err := getMPD()
		if err != nil {
			return c.JSON(fiber.Map{"playlists": []fiber.Map{}})
		}
		lines, _ := m.raw("listplaylists")
		res := []fiber.Map{}
		for _, l := range lines {
			if strings.HasPrefix(l, "playlist: ") {
				res = append(res, fiber.Map{"name": strings.TrimPrefix(l, "playlist: ")})
			}
		}
		return c.JSON(fiber.Map{"playlists": res})
	})
	app.Post("/saved-playlists", func(c fiber.Ctx) error {
		var b struct {
			Name  string   `json:"name"`
			Files []string `json:"files"`
		}
		if err := c.Bind().JSON(&b); err != nil || !validPlaylistName(b.Name) {
			return c.Status(400).JSON(fiber.Map{"error": "valid name required"})
		}
		if len(b.Files) > 5000 {
			return c.Status(400).JSON(fiber.Map{"error": "too many files"})
		}
		relFiles := make([]string, 0, len(b.Files))
		for _, f := range b.Files {
			if f == "" {
				continue
			}
			rel, ok := relMusicPath(f)
			if !ok {
				return c.Status(400).JSON(fiber.Map{"error": "invalid file path"})
			}
			relFiles = append(relFiles, rel)
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		if len(b.Files) > 0 {
			// Создаём плейлист из переданных файлов
			if err := m.exec("playlistclear", b.Name); err != nil {
				return c.Status(502).JSON(fiber.Map{"error": "playlist update failed"})
			}
			added := 0
			for _, f := range relFiles {
				if err := m.exec("playlistadd", b.Name, f); err != nil {
					log.Printf("playlistadd %q %q: %v", b.Name, f, err)
					continue
				}
				added++
			}
			return c.JSON(fiber.Map{"status": "ok", "count": added})
		}
		if err := m.exec("save", b.Name); err != nil {
			log.Printf("playlist save %q: %v", b.Name, err)
			return c.Status(502).JSON(fiber.Map{"error": "playlist save failed"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/saved-playlists/:name/play", func(c fiber.Ctx) error {
		name, err := url.PathUnescape(c.Params("name"))
		if err != nil || !validPlaylistName(name) {
			return c.Status(400).JSON(fiber.Map{"error": "invalid playlist name"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		queueMu.Lock()
		defer queueMu.Unlock()
		if err := m.exec("clear"); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "queue update failed"})
		}
		if err := m.exec("load", name); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "queue update failed"})
		}
		if err := m.exec("play"); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "play failed"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/saved-playlists/:name", func(c fiber.Ctx) error {
		name, err := url.PathUnescape(c.Params("name"))
		if err != nil || !validPlaylistName(name) {
			return c.Status(400).JSON(fiber.Map{"error": "invalid playlist name"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		lines, _ := m.raw("listplaylistinfo", name)
		return c.JSON(fiber.Map{"name": name, "tracks": parseTrackList(lines)})
	})
	app.Delete("/saved-playlists/:name", func(c fiber.Ctx) error {
		name, err := url.PathUnescape(c.Params("name"))
		if err != nil || !validPlaylistName(name) {
			return c.Status(400).JSON(fiber.Map{"error": "invalid playlist name"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		if err := m.exec("rm", name); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "playlist delete failed"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/saved-playlists/:name/add", func(c fiber.Ctx) error {
		name, err := url.PathUnescape(c.Params("name"))
		if err != nil || !validPlaylistName(name) {
			return c.Status(400).JSON(fiber.Map{"error": "invalid playlist name"})
		}
		var b struct {
			File string `json:"file"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.File == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file required"})
		}
		rel, ok := relMusicPath(b.File)
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid file path"})
		}
		if err := mpdExecErr("playlistadd", name, rel); err != nil {
			log.Printf("playlistadd %q: %v", name, err)
			return c.Status(502).JSON(fiber.Map{"error": "playlist update failed"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	// Алиас /saved-playlists/:name/tracks — добавление трека в плейлист (кнопка «➕ В плейлист»).
	app.Post("/saved-playlists/:name/tracks", func(c fiber.Ctx) error {
		name, err := url.PathUnescape(c.Params("name"))
		if err != nil || !validPlaylistName(name) {
			return c.Status(400).JSON(fiber.Map{"error": "invalid playlist name"})
		}
		var b struct {
			File string `json:"file"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.File == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file required"})
		}
		rel, ok := relMusicPath(b.File)
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid file path"})
		}
		if err := mpdExecErr("playlistadd", name, rel); err != nil {
			log.Printf("playlistadd %q: %v", name, err)
			return c.Status(502).JSON(fiber.Map{"error": "playlist update failed"})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Get("/albums", func(c fiber.Ctx) error {
		sortParam := c.Query("sort", "artist")
		albums, err := getAlbumsSorted(sortParam)
		if err != nil || albums == nil {
			albums = []fiber.Map{}
		}
		return c.JSON(fiber.Map{"albums": albums})
	})

	app.Get("/albums/:artist/:album", func(c fiber.Ctx) error {
		artist, _ := url.PathUnescape(c.Params("artist"))
		album, _ := url.PathUnescape(c.Params("album"))
		tracks, err := albumTracks(artist, album)
		if err != nil || tracks == nil {
			tracks = []fiber.Map{}
		}
		return c.JSON(fiber.Map{"tracks": tracks})
	})

	app.Post("/play/album", func(c fiber.Ctx) error {
		var b struct {
			Artist string `json:"artist"`
			Album  string `json:"album"`
		}
		if err := c.Bind().JSON(&b); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		if strings.TrimSpace(b.Artist) == "" || strings.TrimSpace(b.Album) == "" {
			return c.Status(400).JSON(fiber.Map{"error": "artist and album required"})
		}
		if err := playAlbum(b.Artist, b.Album); err != nil {
			log.Printf("play album %q/%q: %v", b.Artist, b.Album, err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "play failed"})
		}
		// Инвалидация кэша треков альбома после запуска воспроизведения (best-effort).
		if rdb != nil {
			_ = rdb.Del(ctx, "music:tracks:"+url.QueryEscape(b.Artist)+":"+url.QueryEscape(b.Album)).Err()
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Post("/play-file", func(c fiber.Ctx) error {
		var b struct {
			File string `json:"file"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.File == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file required"})
		}
		rel, ok := relMusicPath(b.File)
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid file path"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		added, err := replaceQueueAndPlay(m, []string{rel})
		if err != nil {
			log.Printf("play-file %q: %v", rel, err)
			return c.Status(502).JSON(fiber.Map{"error": "queue update failed", "added": added})
		}
		return c.JSON(fiber.Map{"status": "playing", "file": rel})
	})

	app.Get("/covers/:artist/:album", func(c fiber.Ctx) error {
		artist, ok := coverParam(c, "artist")
		if !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid artist"})
		}
		album, ok := coverParam(c, "album")
		if !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid album"})
		}
		if p := coverFile(artist, album); p != "" {
			ct := imageContentType(p)
			if ct == "" {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
			}
			c.Set("Content-Type", ct)
			c.Set("X-Content-Type-Options", "nosniff")
			c.Set("Content-Disposition", `inline; filename="cover"`)
			c.Set("Cache-Control", "public, max-age=86400")
			return c.SendFile(p)
		}
		// Локальной обложки нет: iTunes (работает в РФ) → MusicBrainz.
		// URL из внешнего API проверяется allowlist'ом (иначе open redirect).
		if u := itunesCoverURL(artist, album); u != "" {
			c.Set("Cache-Control", "public, max-age=600")
			return c.Redirect().Status(fiber.StatusFound).To(u)
		}
		if u := musicBrainzCoverURL(artist, album); u != "" {
			c.Set("Cache-Control", "public, max-age=600")
			return c.Redirect().Status(fiber.StatusFound).To(u)
		}
		c.Set("Content-Type", "image/png")
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Cache-Control", "public, max-age=60")
		return c.Send([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82})
	})

	// Нормализованные метаданные альбома из MusicBrainz.
	app.Get("/metadata/:artist/:album", func(c fiber.Ctx) error {
		artist, ok := coverParam(c, "artist")
		if !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid artist"})
		}
		album, ok := coverParam(c, "album")
		if !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid album"})
		}
		if it, err := fetchITunesAlbum(artist, album); err == nil {
			year := ""
			if len(it.ReleaseDate) >= 4 {
				year = it.ReleaseDate[:4]
			}
			return c.JSON(fiber.Map{
				"artist": it.ArtistName, "album": it.CollectionName, "year": year,
				"cover": itunesArtworkURL(it.ArtworkURL100), "source": "itunes",
			})
		}
		rel, err := fetchMusicBrainzRelease(artist, album)
		if err != nil {
			log.Printf("metadata %q/%q: %v", artist, album, err)
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "not found"})
		}
		year := ""
		if len(rel.Date) >= 4 {
			year = rel.Date[:4]
		}
		name := artist
		if len(rel.ArtistCredit) > 0 {
			name = rel.ArtistCredit[0].Name
		}
		return c.JSON(fiber.Map{
			"artist": name, "album": rel.Title, "year": year,
			"mbid": rel.ID, "source": "musicbrainz",
			"cover": coverArtArchiveURL(rel.ID),
		})
	})

	// «Моя волна» — случайный трек того же артиста, что играет сейчас
	app.Get("/radio/next", func(c fiber.Ctx) error {
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		cur := fieldsToMap(m.fields("currentsong"))
		artist, _ := cur["artist"].(string)
		curFile, _ := cur["file"].(string)
		if artist == "" {
			return c.Status(400).JSON(fiber.Map{"error": "нет текущего трека"})
		}
		tracks := mpdSearch(artist)
		candidates := []fiber.Map{}
		for _, t := range tracks {
			f, _ := t["file"].(string)
			if f != "" && f != curFile {
				candidates = append(candidates, t)
			}
		}
		if len(candidates) == 0 {
			return c.JSON(fiber.Map{"status": "no_more", "artist": artist})
		}
		idx := int(time.Now().UnixNano() % int64(len(candidates)))
		if idx < 0 {
			idx = 0
		}
		next := candidates[idx]
		file, _ := next["file"].(string)
		if _, err := replaceQueueAndPlay(m, []string{file}); err != nil {
			return c.Status(502).JSON(fiber.Map{"error": "queue update failed"})
		}
		return c.JSON(fiber.Map{"status": "ok", "title": next["title"], "artist": artist})
	})

	app.Get("/search", func(c fiber.Ctx) error {
		q := strings.TrimSpace(c.Query("q"))
		if q == "" {
			return c.JSON(fiber.Map{"results": []fiber.Map{}})
		}
		if len(q) > 200 {
			return c.Status(400).JSON(fiber.Map{"error": "query too long"})
		}
		res := mpdSearch(q)
		truncated := false
		if len(res) > 500 {
			res = res[:500]
			truncated = true
		}
		return c.JSON(fiber.Map{"results": res, "truncated": truncated})
	})

	// ==================== ТЕКСТ ПЕСНИ (LRCLIB) ====================
	app.Get("/lyrics", func(c fiber.Ctx) error {
		artist := clampTag(c.Query("artist"))
		track := clampTag(c.Query("track"))
		album := clampTag(c.Query("album"))
		if artist == "" || track == "" {
			return c.Status(400).JSON(fiber.Map{"error": "artist and track required"})
		}
		cacheKey := lyricsCacheKey(artist, track)
		if rdb != nil {
			if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
				c.Set("Content-Type", "application/json")
				c.Set("X-Content-Type-Options", "nosniff")
				return c.SendString(cached)
			}
		}
		vals := url.Values{
			"artist_name": {artist},
			"track_name":  {track},
			"album_name":  {album},
		}
		if d, err := strconv.Atoi(c.Query("duration", "0")); err == nil && d > 0 && d <= 24*3600 {
			vals.Set("duration", strconv.Itoa(d))
		}
		qctx, cancel := context.WithTimeout(c.Context(), 12*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(qctx, http.MethodGet,
			"https://lrclib.net/api/get?"+vals.Encode(), nil)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "internal error"})
		}
		req.Header.Set("User-Agent", "NAS-Stack/1.0 (https://github.com/FuzzyHD1993/nas-stack)")
		resp, err := lyricsHTTP.Do(req)
		if err != nil {
			log.Printf("lyrics %q/%q: %v", artist, track, err)
			return c.Status(502).JSON(fiber.Map{"error": "lyrics unavailable"})
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxLyricsBody+1))
		if err != nil || len(body) > maxLyricsBody {
			return c.Status(502).JSON(fiber.Map{"error": "lyrics unavailable"})
		}
		if resp.StatusCode != http.StatusOK {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "lyrics not found"})
		}
		if !json.Valid(body) {
			return c.Status(502).JSON(fiber.Map{"error": "lyrics unavailable"})
		}
		if rdb != nil {
			_ = rdb.Set(ctx, cacheKey, string(body), 24*time.Hour).Err()
		}
		c.Set("Content-Type", "application/json")
		c.Set("X-Content-Type-Options", "nosniff")
		return c.Send(body)
	})

	// ==================== ИЗБРАННОЕ ====================
	app.Get("/favorites", func(c fiber.Ctx) error {
		vals, _ := rdb.HGetAll(ctx, "music:favorites").Result()
		items := []fiber.Map{}
		for k, v := range vals {
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(v), &m); err != nil || m == nil {
				continue
			}
			m["file_path"] = k
			items = append(items, m)
		}
		sort.Slice(items, func(i, j int) bool { return fmt.Sprint(items[i]["added_at"]) > fmt.Sprint(items[j]["added_at"]) })
		return c.JSON(fiber.Map{"favorites": items, "count": len(items)})
	})
	app.Post("/favorites", func(c fiber.Ctx) error {
		var b struct {
			FilePath string `json:"file_path"`
			Title    string `json:"title"`
			Artist   string `json:"artist"`
			Album    string `json:"album"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.FilePath == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file_path required"})
		}
		rel, ok := relMusicPath(b.FilePath)
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid file_path"})
		}
		rec := fiber.Map{"title": b.Title, "artist": b.Artist, "album": b.Album, "added_at": time.Now().Format(time.RFC3339)}
		data, _ := json.Marshal(rec)
		_ = rdb.HSet(ctx, "music:favorites", rel, string(data)).Err()
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/favorites/check", func(c fiber.Ctx) error {
		fp := c.Query("file_path")
		if rel, ok := relMusicPath(fp); ok {
			fp = rel
		}
		fav, err := rdb.HExists(ctx, "music:favorites", fp).Result()
		if err != nil || !fav {
			return c.JSON(fiber.Map{"favorite": false})
		}
		return c.JSON(fiber.Map{"favorite": true, "id": fp})
	})
	app.Delete("/favorites/*", func(c fiber.Ctx) error {
		fp, _ := url.PathUnescape(c.Params("*"))
		if rel, ok := relMusicPath(fp); ok {
			fp = rel
		}
		_ = rdb.HDel(ctx, "music:favorites", fp).Err()
		return c.JSON(fiber.Map{"status": "removed"})
	})

	// ==================== РЕЙТИНГ ====================
	app.Post("/ratings", func(c fiber.Ctx) error {
		var b struct {
			FilePath string `json:"file_path"`
			Rating   int    `json:"rating"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.FilePath == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file_path required"})
		}
		if b.Rating < 0 {
			b.Rating = 0
		}
		if b.Rating > 5 {
			b.Rating = 5
		}
		if b.Rating == 0 {
			_ = rdb.HDel(ctx, "music:ratings", b.FilePath).Err()
		} else {
			_ = rdb.HSet(ctx, "music:ratings", b.FilePath, strconv.Itoa(b.Rating)).Err()
		}
		return c.JSON(fiber.Map{"status": "ok", "rating": b.Rating})
	})
	app.Get("/ratings", func(c fiber.Ctx) error {
		fp := c.Query("file_path")
		v, _ := rdb.HGet(ctx, "music:ratings", fp).Int()
		return c.JSON(fiber.Map{"file_path": fp, "rating": v})
	})
	app.Get("/ratings/top", func(c fiber.Ctx) error {
		vals, _ := rdb.HGetAll(ctx, "music:ratings").Result()
		type kv struct {
			k string
			v int
		}
		arr := []kv{}
		for k, v := range vals {
			n, _ := strconv.Atoi(v)
			arr = append(arr, kv{k, n})
		}
		sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
		out := []fiber.Map{}
		for i, e := range arr {
			if i >= 50 {
				break
			}
			out = append(out, fiber.Map{"file": e.k, "rating": e.v})
		}
		return c.JSON(fiber.Map{"tracks": out})
	})

	// ==================== ПОХОЖИЕ ТРЕКИ ====================
	app.Get("/similar", func(c fiber.Ctx) error {
		file := c.Query("file")
		artist := c.Query("artist")
		if artist == "" && file != "" {
			// попытаться определить артиста из текущего
			if m, err := getMPD(); err == nil {
				cur := fieldsToMap(m.fields("currentsong"))
				artist, _ = cur["artist"].(string)
			}
		}
		if artist == "" {
			return c.JSON(fiber.Map{"tracks": []fiber.Map{}})
		}
		all := mpdSearch(artist)
		out := []fiber.Map{}
		for _, t := range all {
			f, _ := t["file"].(string)
			if f != "" && f != file {
				out = append(out, t)
			}
		}
		if len(out) > 20 {
			out = out[:20]
		}
		return c.JSON(fiber.Map{"artist": artist, "tracks": out})
	})

	// ==================== ТОЛЬКО ЭТОТ АРТИСТ ====================
	app.Post("/play/artist", func(c fiber.Ctx) error {
		var b struct {
			Artist string `json:"artist"`
		}
		if err := c.Bind().JSON(&b); err != nil || strings.TrimSpace(b.Artist) == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "artist required"})
		}
		name := normArtist(b.Artist)

		_, byName, idxErr := getArtistIndex()
		tracks, canonical, ok := lookupArtist(byName, name)
		if !ok || len(tracks) == 0 {
			if len(byName) == 0 && idxErr != nil {
				return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artist index unavailable"})
			}
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "artist not found"})
		}
		m, err := getMPD()
		if err != nil {
			log.Printf("play artist: mpd: %v", err)
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "mpd unavailable"})
		}
		files := make([]string, 0, len(tracks))
		for _, t := range tracks {
			files = append(files, t.File)
		}
		added, err := replaceQueueAndPlay(m, files)
		if err != nil {
			log.Printf("play artist %q: %v", name, err)
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "queue update failed", "added": added})
		}
		return c.JSON(fiber.Map{"status": "ok", "artist": canonical, "count": added})
	})

	// ==================== ПЕРЕМЕЩЕНИЕ В ОЧЕРЕДИ ====================
	app.Post("/queue/move", func(c fiber.Ctx) error {
		var b struct {
			From int `json:"from"`
			To   int `json:"to"`
		}
		if err := c.Bind().JSON(&b); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		if b.From < 0 || b.To < 0 || b.From > 1_000_000 || b.To > 1_000_000 {
			return c.Status(400).JSON(fiber.Map{"error": "invalid positions"})
		}
		if err := mpdExecErr("move", strconv.Itoa(b.From), strconv.Itoa(b.To)); err != nil {
			log.Printf("queue move: %v", err)
			return c.Status(400).JSON(fiber.Map{"error": "invalid move"})
		}
		return c.JSON(fiber.Map{"status": "moved"})
	})

	// ==================== ПРОГРЕСС ДЛИННЫХ ТРЕКОВ ====================
	app.Post("/track/progress", func(c fiber.Ctx) error {
		var b struct {
			FilePath    string `json:"file_path"`
			PositionSec int    `json:"position_sec"`
			DurationSec int    `json:"duration_sec"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.FilePath == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file_path required"})
		}
		rel, ok := relMusicPath(b.FilePath)
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid file_path"})
		}
		if b.PositionSec < 0 || b.DurationSec < 0 ||
			b.PositionSec > 24*3600 || b.DurationSec > 24*3600 {
			return c.Status(400).JSON(fiber.Map{"error": "invalid position"})
		}
		data, _ := json.Marshal(fiber.Map{"position_sec": b.PositionSec, "duration_sec": b.DurationSec, "updated_at": time.Now().Format(time.RFC3339)})
		_ = rdb.Set(ctx, progressKey(rel), string(data), 30*24*time.Hour).Err()
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/track/progress", func(c fiber.Ctx) error {
		rel, ok := relMusicPath(c.Query("file_path"))
		if !ok {
			return c.Status(400).JSON(fiber.Map{"error": "invalid file_path"})
		}
		s, err := rdb.Get(ctx, progressKey(rel)).Result()
		if err != nil || s == "" {
			return c.JSON(fiber.Map{"position_sec": 0})
		}
		c.Set("Content-Type", "application/json")
		return c.SendString(s)
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = app.ShutdownWithContext(sctx)
	}()
	log.Println("🎵 music-service на :8092")
	if err := app.Listen(":8092"); err != nil {
		log.Printf("listen: %v", err)
	}
	if db != nil {
		db.Close()
	}
	if rdb != nil {
		_ = rdb.Close()
	}
}

// mpdExecErr — как mpdExec, но возвращает ошибку.
func mpdExecErr(cmd string, args ...string) error {
	m, err := getMPD()
	if err != nil {
		return err
	}
	return m.exec(cmd, args...)
}

// mpdOK отвечает ok только при реальном успехе MPD.
func mpdOK(c fiber.Ctx, cmd string, args ...string) error {
	if err := mpdExecErr(cmd, args...); err != nil {
		log.Printf("mpd %s: %v", cmd, err)
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": "mpd command failed"})
	}
	return c.JSON(fiber.Map{"status": "ok"})
}

// audioExts — расширения аудиофайлов для догрузки папки.
var audioExts = map[string]bool{
	".flac": true, ".mp3": true, ".ogg": true, ".oga": true, ".wav": true,
	".m4a": true, ".aac": true, ".opus": true, ".dsf": true, ".dff": true,
	".ape": true, ".wv": true, ".wma": true,
}

// ensureNextTrack: если текущий трек — последний в очереди (и нет repeat/single),
// догружает остальные аудиофайлы его папки, чтобы команда next не уводила в stop.
func ensureNextTrack(m *mpdClient) {
	st := fieldsToMap(m.fields("status"))
	if st["repeat"] == "1" || st["single"] == "1" {
		return
	}
	songStr, _ := st["song"].(string)
	plStr, _ := st["playlistlength"].(string)
	song, _ := strconv.Atoi(songStr)
	plLen, _ := strconv.Atoi(plStr)
	if plLen == 0 || song+1 < plLen {
		return // следующий трек уже есть
	}

	cur := fieldsToMap(m.fields("currentsong"))
	file, _ := cur["file"].(string)
	if file == "" {
		return
	}
	rel, ok := relMusicPath(file)
	if !ok {
		return
	}
	dir := path.Dir(rel)
	if dir == "." {
		return
	}
	tracks, err := albumAudioFiles(dir)
	if err != nil {
		return
	}

	existing := map[string]bool{}
	if lines, err := m.raw("playlistinfo"); err == nil {
		for _, l := range lines {
			if strings.HasPrefix(l, "file: ") {
				existing[strings.TrimPrefix(l, "file: ")] = true
			}
		}
	}

	added := 0
	for _, track := range tracks {
		if existing[track] {
			continue
		}
		if err := m.exec("add", track); err == nil {
			added++
		}
	}
	if added > 0 {
		log.Printf("➕ Next: догружено %d треков из %s", added, dir)
	}
}
