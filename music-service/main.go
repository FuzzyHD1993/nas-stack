package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
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
var bitPerfectEnabled = true

// mpdClient — минимальный клиент MPD поверх TCP.
type mpdClient struct {
	conn net.Conn
	mu   sync.Mutex
	r    *bufio.Reader
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
	// Дедлайн только на установку сессии (ping), долгие команды raw() не ограничиваем.
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	c := &mpdClient{conn: conn, r: bufio.NewReader(conn)}
	_, err = c.raw("ping")
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return c, nil
}

func mpdQuote(s string) string {
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

func (c *mpdClient) raw(cmd string, args ...string) ([]string, error) {
	line := cmd
	if len(args) > 0 {
		quoted := make([]string, 0, len(args))
		for _, a := range args {
			quoted = append(quoted, mpdQuote(a))
		}
		line += " " + strings.Join(quoted, " ")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.conn, "%s\n", line); err != nil {
		return nil, err
	}
	var out []string
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
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
	mpdDownFor time.Time // backoff после неудачного диала, защита от thundering herd (под dialMu)
)

// pingTimeout проверяет живость соединения с жёстким дедлайном.
// Не требует mpdMu; сериализуется собственным mu клиента.
func (c *mpdClient) pingTimeout(d time.Duration) bool {
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
	_, err = c.raw("ping")
	return err == nil
}

// getMPD возвращает живое соединение с MPD. Инвариант: сетевые операции
// (ping, dial) выполняются ВНЕ mpdMu — глобальный лок удерживается только на
// чтение/подмену указателя, поэтому медленный MPD не блокирует остальные хендлеры.
func getMPD() (*mpdClient, error) {
	mpdMu.Lock()
	c := mpd
	mpdMu.Unlock()
	if c != nil && c.pingTimeout(2*time.Second) {
		return c, nil
	}
	dialMu.Lock()
	defer dialMu.Unlock()
	// Кто-то из конкурентов мог уже переподключиться, пока ждали dialMu.
	mpdMu.Lock()
	c = mpd
	mpdMu.Unlock()
	if c != nil && c.pingTimeout(2*time.Second) {
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
		c.conn.Close()
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
	st["audio_chain_enabled"] = bitPerfectEnabled
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

// probeSourceAudio читает частоту/битность исходного файла через ffprobe.
func probeSourceAudio(path string) (rate, bits int, codec string) {
	if path == "" {
		return 0, 0, ""
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=sample_rate,bits_per_raw_sample,bits_per_sample,codec_name",
		"-of", "default=noprint_wrappers=1", path).Output()
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
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(songFile), "."))
	if ext != "" {
		info.Codec = ext
	}
	if br, ok := status["bitrate"]; ok {
		info.BitrateKbps, _ = strconv.Atoi(br)
	} else if info.SampleRate > 0 && info.Bits > 0 {
		info.BitrateKbps = info.SampleRate * info.Bits * info.Channels / 1000
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
	if !bitPerfectEnabled {
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
	lines, err := c.raw("list", "Album", "group", "AlbumArtist")
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
			albums = append(albums, fiber.Map{"artist": cur, "album": alb, "cover_url": "/api/music/covers/" + cur + "/" + alb})
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
	if err != nil || len(lines) == 0 {
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
	sort.Slice(tracks, func(i, j int) bool {
		ti, _ := strconv.Atoi(tracks[i]["track"].(string))
		tj, _ := strconv.Atoi(tracks[j]["track"].(string))
		return ti < tj
	})
	cacheSetJSON(key, tracks, tracksCacheTTL)
	return tracks, nil
}

func playAlbum(artist, album string) error {
	c, err := getMPD()
	if err != nil {
		return err
	}
	tracks, _ := albumTracks(artist, album)
	c.exec("clear")
	for _, t := range tracks {
		if f, ok := t["file"].(string); ok {
			c.exec("add", f)
		}
	}
	return c.exec("play")
}

func coverFile(artist, album string) string {
	// Полный поиск: файл в папке альбома (30+ имён + рекурсия) → встроенная
	// обложка через ffmpeg. Реализация в covers.go.
	return coverFor(artist, album)
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

// playTracker отслеживает смену трека и ведёт статистику прослушивания.
func playTracker() {
	last := ""
	var lastID int64
	var lastElapsed, lastDuration float64
	lastState := ""
	prevFile := ""
	var prevElapsed, prevDuration float64
	for {
		if m, err := getMPD(); err == nil {
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
			if lastState == "play" && state == "stop" && prevFile != "" &&
				prevDuration > 0 && prevElapsed >= prevDuration-5 {
				autoNextAlbum(m, prevFile)
			}
			key := file
			if key != "" && key != last {
				// Завершаем предыдущую запись, если отыграла >=90%.
				lastID = recordListen(file, artist, alb, title, duration, lastID, lastElapsed, lastDuration)
				last = key
				playStats.Lock()
				if title != "" {
					playStats.tracks[artist+" — "+title]++
				}
				if artist != "" {
					playStats.artists[artist]++
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
		}
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
	bitPerfectEnabled = platform.GetEnv("BIT_PERFECT_ENABLED", "true") == "true"
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
	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

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
		if !bitPerfectEnabled {
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

	app.Post("/play", func(c fiber.Ctx) error { mpdExec("play"); return c.JSON(fiber.Map{"status": "ok"}) })
	app.Post("/pause", func(c fiber.Ctx) error { mpdExec("pause"); return c.JSON(fiber.Map{"status": "ok"}) })
	app.Post("/stop", func(c fiber.Ctx) error { mpdExec("stop"); return c.JSON(fiber.Map{"status": "ok"}) })
	app.Post("/next", func(c fiber.Ctx) error {
		m, err := getMPD()
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"status": "error", "error": err.Error()})
		}
		// Если следующий трек отсутствует (одиночный файл в очереди) —
		// догружаем остальные треки его папки, чтобы next не уходил в stop.
		ensureNextTrack(m)
		_ = m.exec("next")
		time.Sleep(150 * time.Millisecond)
		if state, _ := mpdStatus()["state"].(string); state != "play" {
			_ = m.exec("play")
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/prev", func(c fiber.Ctx) error { mpdExec("previous"); return c.JSON(fiber.Map{"status": "ok"}) })
	app.Post("/clear", func(c fiber.Ctx) error { mpdExec("clear"); return c.JSON(fiber.Map{"status": "ok"}) })
	app.Post("/shuffle", func(c fiber.Ctx) error { mpdExec("shuffle"); return c.JSON(fiber.Map{"status": "ok"}) })

	app.Post("/volume", func(c fiber.Ctx) error {
		var b struct {
			Level int `json:"level"`
		}
		if err := c.Bind().JSON(&b); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		mpdExec("setvol", strconv.Itoa(b.Level))
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Post("/seek", func(c fiber.Ctx) error {
		var b struct {
			Position float64 `json:"position"`
		}
		if err := c.Bind().JSON(&b); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "invalid body"})
		}
		mpdExec("seekcur", strconv.FormatFloat(b.Position, 'f', 1, 64))
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Post("/repeat", func(c fiber.Ctx) error {
		var b struct {
			Enabled bool `json:"enabled"`
		}
		c.Bind().JSON(&b)
		if b.Enabled {
			mpdExec("repeat", "1")
		} else {
			mpdExec("repeat", "0")
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})

	app.Post("/random", func(c fiber.Ctx) error {
		var b struct {
			Enabled bool `json:"enabled"`
		}
		c.Bind().JSON(&b)
		if b.Enabled {
			mpdExec("random", "1")
		} else {
			mpdExec("random", "0")
		}
		return c.JSON(fiber.Map{"status": "ok"})
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
		name := c.Params("name")
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		m.exec("clear")
		m.exec("load", name)
		m.exec("play")
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
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if err := m.exec("add", b.File); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/queue/clear", func(c fiber.Ctx) error {
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		m.exec("clear")
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Delete("/queue/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		m.exec("deleteid", id)
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/queue/play/:id", func(c fiber.Ctx) error {
		id := c.Params("id")
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if err := m.exec("playid", id); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ok"})
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
		if err := c.Bind().JSON(&b); err != nil || b.Name == "" {
			return c.Status(400).JSON(fiber.Map{"error": "name required"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if len(b.Files) > 0 {
			// Создаём плейлист из переданных файлов
			m.exec("playlistclear", b.Name)
			for _, f := range b.Files {
				if f != "" {
					m.exec("playlistadd", b.Name, f)
				}
			}
			return c.JSON(fiber.Map{"status": "ok", "count": len(b.Files)})
		}
		if err := m.exec("save", b.Name); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/saved-playlists/:name/play", func(c fiber.Ctx) error {
		name, _ := url.QueryUnescape(c.Params("name"))
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		m.exec("clear")
		m.exec("load", name)
		m.exec("play")
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/saved-playlists/:name", func(c fiber.Ctx) error {
		name, _ := url.QueryUnescape(c.Params("name"))
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		lines, _ := m.raw("listplaylistinfo", name)
		return c.JSON(fiber.Map{"name": name, "tracks": parseTrackList(lines)})
	})
	app.Delete("/saved-playlists/:name", func(c fiber.Ctx) error {
		name, _ := url.QueryUnescape(c.Params("name"))
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		m.exec("rm", name)
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Post("/saved-playlists/:name/add", func(c fiber.Ctx) error {
		name, _ := url.QueryUnescape(c.Params("name"))
		var b struct {
			File string `json:"file"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.File == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file required"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		m.exec("playlistadd", name, b.File)
		return c.JSON(fiber.Map{"status": "ok"})
	})
	// Алиас /saved-playlists/:name/tracks — добавление трека в плейлист (кнопка «➕ В плейлист»).
	app.Post("/saved-playlists/:name/tracks", func(c fiber.Ctx) error {
		name, _ := url.QueryUnescape(c.Params("name"))
		var b struct {
			File string `json:"file"`
		}
		if err := c.Bind().JSON(&b); err != nil || b.File == "" {
			return c.Status(400).JSON(fiber.Map{"error": "file required"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if err := m.exec("playlistadd", name, b.File); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
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
		artist, _ := url.QueryUnescape(c.Params("artist"))
		album, _ := url.QueryUnescape(c.Params("album"))
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
		if err := playAlbum(b.Artist, b.Album); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
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
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if err := m.exec("clear"); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if err := m.exec("add", b.File); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		if err := m.exec("play"); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "playing", "file": b.File})
	})

	app.Get("/covers/:artist/:album", func(c fiber.Ctx) error {
		artist, _ := url.QueryUnescape(c.Params("artist"))
		album, _ := url.QueryUnescape(c.Params("album"))
		p := coverFile(artist, album)
		if p == "" {
			// Локальной обложки нет: iTunes (работает в РФ) → MusicBrainz.
			if u := itunesCoverURL(artist, album); u != "" {
				return c.Redirect().Status(fiber.StatusFound).To(u)
			}
			if u := musicBrainzCoverURL(artist, album); u != "" {
				return c.Redirect().Status(fiber.StatusFound).To(u)
			}
			c.Set("Content-Type", "image/png")
			return c.Send([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82})
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".png":
			c.Set("Content-Type", "image/png")
		case ".webp":
			c.Set("Content-Type", "image/webp")
		default:
			c.Set("Content-Type", "image/jpeg")
		}
		c.Set("Cache-Control", "public, max-age=86400")
		return c.SendFile(p)
	})

	// Нормализованные метаданные альбома из MusicBrainz.
	app.Get("/metadata/:artist/:album", func(c fiber.Ctx) error {
		artist, _ := url.QueryUnescape(c.Params("artist"))
		album, _ := url.QueryUnescape(c.Params("album"))
		if it, err := fetchITunesAlbum(artist, album); err == nil {
			year := ""
			if len(it.ReleaseDate) >= 4 {
				year = it.ReleaseDate[:4]
			}
			return c.JSON(fiber.Map{
				"artist": it.ArtistName, "album": it.CollectionName, "year": year,
				"cover": itunesCoverURL(artist, album), "source": "itunes",
			})
		}
		rel, err := fetchMusicBrainzRelease(artist, album)
		if err != nil {
			return c.Status(404).JSON(fiber.Map{"error": err.Error()})
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
			"cover": "https://coverartarchive.org/release/" + rel.ID + "/front-500",
		})
	})

	// «Моя волна» — случайный трек того же артиста, что играет сейчас
	app.Get("/radio/next", func(c fiber.Ctx) error {
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
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
		m.exec("clear")
		m.exec("add", file)
		m.exec("play")
		return c.JSON(fiber.Map{"status": "ok", "title": next["title"], "artist": artist})
	})

	app.Get("/search", func(c fiber.Ctx) error {
		q := c.Query("q")
		if q == "" {
			return c.JSON(fiber.Map{"results": []fiber.Map{}})
		}
		return c.JSON(fiber.Map{"results": mpdSearch(q)})
	})

	// ==================== ТЕКСТ ПЕСНИ (LRCLIB) ====================
	app.Get("/lyrics", func(c fiber.Ctx) error {
		artist := c.Query("artist")
		track := c.Query("track")
		album := c.Query("album")
		duration := c.Query("duration", "0")
		if artist == "" || track == "" {
			return c.Status(400).JSON(fiber.Map{"error": "artist and track required"})
		}
		cacheKey := "lyrics:" + artist + ":" + track
		if cached, err := rdb.Get(ctx, cacheKey).Result(); err == nil && cached != "" {
			c.Set("Content-Type", "application/json")
			return c.SendString(cached)
		}
		vals := url.Values{
			"artist_name": {artist},
			"track_name":  {track},
			"album_name":  {album},
		}
		if d, _ := strconv.Atoi(duration); d > 0 {
			vals.Set("duration", strconv.Itoa(d))
		}
		u := "https://lrclib.net/api/get?" + vals.Encode()
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", "NAS-Stack/1.0 (https://github.com/FuzzyHD1993/nas-stack)")
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			return c.Status(502).JSON(fiber.Map{"error": err.Error()})
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == 200 {
			_ = rdb.Set(ctx, cacheKey, string(body), 24*time.Hour).Err()
		}
		c.Set("Content-Type", "application/json")
		return c.Status(resp.StatusCode).Send(body)
	})

	// ==================== ИЗБРАННОЕ ====================
	app.Get("/favorites", func(c fiber.Ctx) error {
		vals, _ := rdb.HGetAll(ctx, "music:favorites").Result()
		items := []fiber.Map{}
		for k, v := range vals {
			var m map[string]interface{}
			if json.Unmarshal([]byte(v), &m) == nil {
				m["file_path"] = k
				items = append(items, m)
			}
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
		rec := fiber.Map{"title": b.Title, "artist": b.Artist, "album": b.Album, "added_at": time.Now().Format(time.RFC3339)}
		data, _ := json.Marshal(rec)
		_ = rdb.HSet(ctx, "music:favorites", b.FilePath, string(data)).Err()
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/favorites/check", func(c fiber.Ctx) error {
		fp := c.Query("file_path")
		fav, err := rdb.HExists(ctx, "music:favorites", fp).Result()
		if err != nil || !fav {
			return c.JSON(fiber.Map{"favorite": false})
		}
		return c.JSON(fiber.Map{"favorite": true, "id": fp})
	})
	app.Delete("/favorites/*", func(c fiber.Ctx) error {
		fp, _ := url.QueryUnescape(c.Params("*"))
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
		file, _ := url.QueryUnescape(c.Query("file"))
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
		if err := c.Bind().JSON(&b); err != nil || b.Artist == "" {
			return c.Status(400).JSON(fiber.Map{"error": "artist required"})
		}
		m, err := getMPD()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		m.exec("clear")
		m.exec("searchadd", "Artist", b.Artist)
		m.exec("play")
		return c.JSON(fiber.Map{"status": "ok", "artist": b.Artist})
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
		if err := mpdExecErr("move", strconv.Itoa(b.From), strconv.Itoa(b.To)); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
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
		data, _ := json.Marshal(fiber.Map{"position_sec": b.PositionSec, "duration_sec": b.DurationSec, "updated_at": time.Now().Format(time.RFC3339)})
		_ = rdb.Set(ctx, "music:progress:"+b.FilePath, string(data), 30*24*time.Hour).Err()
		return c.JSON(fiber.Map{"status": "ok"})
	})
	app.Get("/track/progress", func(c fiber.Ctx) error {
		fp := c.Query("file_path")
		s, err := rdb.Get(ctx, "music:progress:"+fp).Result()
		if err != nil || s == "" {
			return c.JSON(fiber.Map{"position_sec": 0})
		}
		c.Set("Content-Type", "application/json")
		return c.SendString(s)
	})

	log.Println("🎵 music-service на :8092")
	log.Fatal(app.Listen(":8092"))
}

// mpdExecErr — как mpdExec, но возвращает ошибку.
func mpdExecErr(cmd string, args ...string) error {
	m, err := getMPD()
	if err != nil {
		return err
	}
	return m.exec(cmd, args...)
}

// mpdExec — выполнить команду на глобальном клиенте, игнорируя ошибки.
func mpdExec(cmd string, args ...string) {
	m, err := getMPD()
	if err != nil {
		return
	}
	m.exec(cmd, args...)
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
	dir := filepath.Dir(file)
	musicDir := platform.GetEnv("MUSIC_DIR", "/mnt/nas/root/FuzzyHD/Music")
	entries, err := os.ReadDir(filepath.Join(musicDir, dir))
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
	for _, e := range entries {
		if e.IsDir() || !audioExts[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(dir, e.Name()))
		if existing[rel] {
			continue
		}
		if err := m.exec("add", rel); err == nil {
			added++
		}
	}
	if added > 0 {
		log.Printf("➕ Next: догружено %d треков из %s", added, dir)
	}
}
