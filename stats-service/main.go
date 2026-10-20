package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

var rdb *redis.Client

var nasCache = struct {
	sync.Mutex
	data     fiber.Map
	expires  time.Time
	updating bool
}{}

const (
	nasCacheKey = "stats:nas:cache"
	nasCacheTTL = 10 * time.Minute
)

// computeNasDirs — медленный du -sh по CIFS; вызывать только в фоне.
func computeNasDirs() []fiber.Map {
	dirs := []string{
		platform.GetEnv("MEDIA_MOVIES_HOST_DIR", "/mnt/nas/root/FuzzyHD/Movies"),
		platform.GetEnv("MEDIA_TV_HOST_DIR", "/mnt/nas/root/FuzzyHD/TV"),
		platform.GetEnv("MUSIC_HOST_DIR", "/mnt/nas/root/FuzzyHD/Music"),
		platform.GetEnv("YOUTUBE_HOST_DIR", "/mnt/nas/YT_video"),
	}
	result := []fiber.Map{}
	for _, d := range dirs {
		size := "—"
		if out, err := exec.Command("du", "-sh", d).Output(); err == nil {
			f := strings.Fields(string(out))
			if len(f) > 0 {
				size = f[0]
			}
		}
		result = append(result, fiber.Map{"path": d, "size": size})
	}
	return result
}

// startNasUpdate запускает фоновое обновление (du по CIFS), не блокируя запрос.
func startNasUpdate() {
	nasCache.Lock()
	if nasCache.updating {
		nasCache.Unlock()
		return
	}
	nasCache.updating = true
	nasCache.Unlock()

	go func() {
		data := fiber.Map{
			"dirs":       computeNasDirs(),
			"cached":     true,
			"updated_at": time.Now().UTC().Format(time.RFC3339),
		}
		if rdb != nil {
			if b, err := json.Marshal(data); err == nil {
				_ = rdb.Set(context.Background(), nasCacheKey, b, nasCacheTTL).Err()
			}
		}
		nasCache.Lock()
		nasCache.data = data
		nasCache.expires = time.Now().Add(nasCacheTTL)
		nasCache.updating = false
		nasCache.Unlock()
	}()
}

func main() {
	rdb = redis.NewClient(&redis.Options{
		Addr: platform.GetEnv("REDIS_ADDR", "localhost:6379"),
	})

	go diskWatchdog()

	// Прогрев кэша /nas в фоне (du по CIFS может занять 30+ сек).
	go func() {
		time.Sleep(5 * time.Second)
		startNasUpdate()
	}()

	app := fiber.New()
	platform.StartPprof()
	app.Use(metricsMiddleware())

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error { return c.JSON(platform.HealthJSON("stats", nil)) })

	app.Get("/all", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"cpu":   getCPU(),
			"ram":   getRAM(),
			"disks": getDisks(),
			"gpu":   getGPU(),
		})
	})

	app.Get("/cpu", func(c fiber.Ctx) error { return c.JSON(getCPU()) })
	app.Get("/ram", func(c fiber.Ctx) error { return c.JSON(getRAM()) })
	app.Get("/disks", func(c fiber.Ctx) error { return c.JSON(getDisks()) })
	app.Get("/gpu", func(c fiber.Ctx) error { return c.JSON(getGPU()) })
	app.Get("/load", func(c fiber.Ctx) error { return c.JSON(getLoad()) })
	app.Get("/cpu/cores", func(c fiber.Ctx) error { return c.JSON(getCPUCores()) })
	app.Get("/network", func(c fiber.Ctx) error { return c.JSON(getNetwork()) })
	app.Get("/processes", func(c fiber.Ctx) error {
		limit, _ := strconv.Atoi(c.Query("limit", "10"))
		if limit <= 0 || limit > 50 {
			limit = 10
		}
		return c.JSON(getProcesses(limit))
	})

	// Прокси к host-agent (сенсоры, SMART, очистка)
	app.Get("/sensors", proxyHostAgent("/sensors"))
	app.Get("/smart", proxyHostAgent("/smart"))
	app.Get("/temp", func(c fiber.Ctx) error {
		body, err := hostAgentGet("/sensors")
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "host-agent unavailable"})
		}
		return c.Send(body)
	})
	app.Get("/cleanup/preview", proxyHostAgent("/cleanup/preview"))
	app.Post("/cleanup/run", func(c fiber.Ctx) error {
		qs := string(c.Request().URI().QueryString())
		target := hostAgentURL() + "/cleanup/run"
		if qs != "" {
			target += "?" + qs
		}
		// host-agent закрыл host-exec ручки (podman prune, rm -rf) за ADMIN_TOKEN —
		// сервисный вызов тоже должен его предъявить.
		req, err := http.NewRequest("POST", target, strings.NewReader(string(c.Body())))
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		req.Header.Set("Content-Type", "application/json")
		if t := strings.TrimSpace(platform.GetEnv("ADMIN_TOKEN", "")); t != "" {
			req.Header.Set("X-Admin-Token", t)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "host-agent unavailable"})
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		c.Set("Content-Type", "application/json")
		return c.Status(resp.StatusCode).Send(b)
	})
	app.Get("/nas", func(c fiber.Ctx) error {
		nasCache.Lock()
		data := nasCache.data
		expired := time.Now().After(nasCache.expires)
		nasCache.Unlock()

		if data != nil && !expired {
			return c.JSON(data)
		}
		// Redis-кэш (10 мин) — общий и переживает рестарт контейнера.
		if rdb != nil {
			if b, err := rdb.Get(context.Background(), nasCacheKey).Bytes(); err == nil {
				var d fiber.Map
				if json.Unmarshal(b, &d) == nil && d != nil {
					nasCache.Lock()
					nasCache.data = d
					nasCache.expires = time.Now().Add(nasCacheTTL)
					nasCache.Unlock()
					return c.JSON(d)
				}
			}
		}
		// Есть устаревшие данные — отдаём их сразу, обновляем в фоне.
		if data != nil {
			startNasUpdate()
			return c.JSON(data)
		}
		// Холодный старт: мгновенная заглушка + фоновый du.
		startNasUpdate()
		return c.JSON(fiber.Map{"dirs": []fiber.Map{}, "cached": false})
	})

	log.Println("📊 stats-service на :8082")
	log.Fatal(app.Listen(":8082"))
}

func getCPU() fiber.Map {
	model := "Unknown"
	data, _ := os.ReadFile("/proc/cpuinfo")
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				model = strings.TrimSpace(parts[1])
				break
			}
		}
	}
	return fiber.Map{
		"model": model,
		"usage": readCPUUsage(),
	}
}

func readCPUUsage() float64 {
	read := func() (idle, total uint64) {
		data, _ := os.ReadFile("/proc/stat")
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "cpu ") {
				fields := strings.Fields(line)[1:]
				for i, f := range fields {
					v, _ := strconv.ParseUint(f, 10, 64)
					if i == 3 {
						idle = v
					}
					total += v
				}
				return
			}
		}
		return
	}
	idle1, total1 := read()
	time.Sleep(200 * time.Millisecond)
	idle2, total2 := read()
	totalDiff := float64(total2 - total1)
	idleDiff := float64(idle2 - idle1)
	if totalDiff == 0 {
		return 0
	}
	return (totalDiff - idleDiff) / totalDiff * 100
}

func getRAM() fiber.Map {
	data, _ := os.ReadFile("/proc/meminfo")
	var total, available float64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			total = val / 1024 / 1024
		case "MemAvailable:":
			available = val / 1024 / 1024
		}
	}
	used := total - available
	pct := 0.0
	if total > 0 {
		pct = used / total * 100
	}
	return fiber.Map{
		"used_gb":  fmt.Sprintf("%.2f", used),
		"total_gb": fmt.Sprintf("%.2f", total),
		"percent":  fmt.Sprintf("%.1f", pct),
	}
}

// realFS — «настоящие» файловые системы (в отличие от псевдо-ФС ядра).
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"btrfs": true, "xfs": true, "f2fs": true, "jfs": true, "reiserfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
	"cifs": true, "smb3": true, "nfs": true, "nfs4": true,
	"zfs": true, "fuseblk": true, "bcachefs": true,
}

// getDisks возвращает реальные диски/шары хоста.
//
// Если корень хоста смонтирован в /host-root (pid:host + volume),
// читаем таблицу монтирования хоста, иначе — fallback через df внутри контейнера.
func getDisks() []fiber.Map {
	if _, err := os.Stat("/host-root/proc/mounts"); err == nil {
		if res := hostDisks(); len(res) > 0 {
			return res
		}
	}
	return localDisks()
}

// hostDisks читает таблицу монтирования (корень хоста виден как /host-root)
// и считает место через statfs. Одна ФС — одна строка (дедуп по устройству).
func hostDisks() []fiber.Map {
	data, err := os.ReadFile("/host-root/proc/mounts")
	if err != nil || len(data) == 0 {
		data, err = os.ReadFile("/proc/mounts")
		if err != nil {
			return nil
		}
	}

	type disk struct {
		mount  string
		fstype string
		device string
		total  uint64
		used   uint64
		avail  uint64
		pct    float64
	}
	list := []disk{}
	byDevice := map[string]int{}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		device, mp, fstype := fields[0], fields[1], fields[2]
		if !realFS[fstype] || !strings.HasPrefix(mp, "/host-root") {
			continue
		}
		hostMount := strings.TrimPrefix(mp, "/host-root")
		if hostMount == "" {
			hostMount = "/"
		}
		hostMount = unescapeMount(hostMount)

		total, used, avail, pct, ok := diskUsage(mp)
		if !ok {
			continue
		}
		if idx, seen := byDevice[device]; seen {
			// одна ФС (напр. btrfs-субтома) — оставляем самое короткое имя
			if len(hostMount) < len(list[idx].mount) {
				list[idx] = disk{hostMount, fstype, device, total, used, avail, pct}
			}
			continue
		}
		byDevice[device] = len(list)
		list = append(list, disk{hostMount, fstype, device, total, used, avail, pct})
	}

	sort.SliceStable(list, func(i, j int) bool { return mountRank(list[i].mount) < mountRank(list[j].mount) })

	result := []fiber.Map{}
	for _, d := range list {
		result = append(result, fiber.Map{
			"mount":   d.mount,
			"fstype":  d.fstype,
			"device":  d.device,
			"used":    humanSize(d.used),
			"total":   humanSize(d.total),
			"free":    humanSize(d.avail),
			"percent": fmt.Sprintf("%.0f%%", d.pct),
		})
	}
	return result
}

// localDisks — fallback: df внутри контейнера с фильтрацией мусора.
func localDisks() []fiber.Map {
	out, _ := exec.Command("df", "-h", "--output=fstype,target,used,size,pcent").Output()
	result := []fiber.Map{}
	for _, line := range strings.Split(string(out), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		fstype, mount := fields[0], fields[1]
		if !realFS[fstype] {
			continue
		}
		result = append(result, fiber.Map{
			"mount":   mount,
			"fstype":  fstype,
			"used":    fields[2],
			"total":   fields[3],
			"percent": fields[4],
		})
	}
	return result
}

// diskUsage возвращает размер/занято/процент для пути через statfs.
// diskUsage возвращает total/used/avail/процент по формуле, как у df:
// used = (Blocks-Bfree), avail = Bavail, percent = used/(used+avail).
func diskUsage(path string) (total, used, avail uint64, pct float64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, 0, 0, false
	}
	bsize := uint64(st.Bsize)
	total = st.Blocks * bsize
	if total == 0 {
		return 0, 0, 0, 0, false
	}
	used = (st.Blocks - st.Bfree) * bsize
	avail = st.Bavail * bsize
	denom := used + avail
	if denom == 0 {
		return total, used, avail, 0, true
	}
	pct = float64(used) / float64(denom) * 100
	return total, used, avail, pct, true
}

// humanSize форматирует байты в человекочитаемый вид (K/M/G/T).
func humanSize(b uint64) string {
	const k = 1024
	f := float64(b)
	units := []string{"B", "K", "M", "G", "T", "P"}
	i := 0
	for f >= k && i < len(units)-1 {
		f /= k
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", b)
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}

// mountRank — порядок сортировки: корень, NAS-шары, загрузочные, прочее.
func mountRank(m string) int {
	switch {
	case m == "/":
		return 0
	case strings.HasPrefix(m, "/mnt/nas"):
		return 1
	case m == "/boot" || m == "/boot/efi":
		return 2
	default:
		return 3
	}
}

// unescapeMount раскодирует octal-escape (\040 и т.п.) из /proc/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func getLoad() fiber.Map {
	data, _ := os.ReadFile("/proc/loadavg")
	fields := strings.Fields(string(data))
	upData, _ := os.ReadFile("/proc/uptime")
	upFields := strings.Fields(string(upData))
	res := fiber.Map{"uptime_sec": 0}
	if len(upFields) > 0 {
		if u, err := strconv.ParseFloat(upFields[0], 64); err == nil {
			res["uptime_sec"] = int(u)
		}
	}
	if len(fields) >= 3 {
		l1, _ := strconv.ParseFloat(fields[0], 64)
		l5, _ := strconv.ParseFloat(fields[1], 64)
		l15, _ := strconv.ParseFloat(fields[2], 64)
		res["load1"] = l1
		res["load5"] = l5
		res["load15"] = l15
	}
	return res
}

func getCPUCores() []fiber.Map {
	read := func() map[string][]uint64 {
		cores := map[string][]uint64{}
		data, _ := os.ReadFile("/proc/stat")
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "cpu") || len(line) < 4 {
				continue
			}
			if line[3] < '0' || line[3] > '9' {
				continue
			}
			fields := strings.Fields(line)
			nums := []uint64{}
			for _, f := range fields[1:] {
				v, _ := strconv.ParseUint(f, 10, 64)
				nums = append(nums, v)
			}
			cores[fields[0]] = nums
		}
		return cores
	}
	first := read()
	time.Sleep(200 * time.Millisecond)
	second := read()
	result := []fiber.Map{}
	for id, nums2 := range second {
		nums1 := first[id]
		if len(nums1) == 0 || len(nums2) == 0 {
			continue
		}
		var total1, idle1, total2, idle2 uint64
		for i, v := range nums1 {
			total1 += v
			if i == 3 {
				idle1 = v
			}
		}
		for i, v := range nums2 {
			total2 += v
			if i == 3 {
				idle2 = v
			}
		}
		td := float64(total2 - total1)
		idleDiff := float64(idle2 - idle1)
		usage := 0.0
		if td > 0 {
			usage = (td - idleDiff) / td * 100
		}
		result = append(result, fiber.Map{"id": id, "usage": fmt.Sprintf("%.1f", usage)})
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(result[i]["id"].(string), "cpu"))
		b, _ := strconv.Atoi(strings.TrimPrefix(result[j]["id"].(string), "cpu"))
		return a < b
	})
	return result
}

func getNetwork() []fiber.Map {
	data, _ := os.ReadFile("/proc/net/dev")
	result := []fiber.Map{}
	for _, line := range strings.Split(string(data), "\n")[2:] {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		if iface == "lo" {
			continue
		}
		result = append(result, fiber.Map{"iface": iface, "rx_bytes": rx, "tx_bytes": tx})
	}
	return result
}

// getProcesses возвращает топ процессов по CPU. Требует pid:host в контейнере,
// иначе видны только процессы самого контейнера.
func getProcesses(limit int) []fiber.Map {
	out, err := exec.Command("ps", "-eo", "pid,comm,%cpu,%mem", "--sort=-%cpu").Output()
	if err != nil {
		out, _ = exec.Command("ps", "aux", "--sort=-%cpu").Output()
	}
	result := []fiber.Map{}
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		name := fields[1]
		// пропускаем сам ps и наш сервис
		if name == "ps" || name == "stats-service" {
			continue
		}
		result = append(result, fiber.Map{
			"pid":  fields[0],
			"name": name,
			"cpu":  fields[len(fields)-2],
			"mem":  fields[len(fields)-1],
		})
		if len(result) >= limit {
			break
		}
	}
	return result
}

func getGPU() fiber.Map {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=name,temperature.gpu,utilization.gpu,memory.used,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return fiber.Map{"error": "nvidia-smi not available"}
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ", ")
	if len(parts) < 5 {
		return fiber.Map{"error": "unexpected output"}
	}
	return fiber.Map{
		"name":         parts[0],
		"temp":         parts[1] + "°C",
		"usage":        parts[2] + "%",
		"memory_used":  parts[3] + " MB",
		"memory_total": parts[4] + " MB",
	}
}

// diskWatchdog периодически проверяет заполненность дисков и публикует
// предупреждения в Redis (диск > DISK_WARN_PERCENT%).
func diskWatchdog() {
	interval := 30
	if v, err := strconv.Atoi(platform.GetEnv("DISK_WATCH_INTERVAL_MIN", "30")); err == nil && v > 0 {
		interval = v
	}
	warnPct := 80.0
	if v, err := strconv.ParseFloat(platform.GetEnv("DISK_WARN_PERCENT", "80"), 64); err == nil && v > 0 {
		warnPct = v
	}
	critPct := 90.0
	if v, err := strconv.ParseFloat(platform.GetEnv("DISK_CRIT_PERCENT", "90"), 64); err == nil && v > 0 {
		critPct = v
	}
	ctx := context.Background()
	log.Printf("💽 Disk watchdog: каждые %d мин, WARN %.0f%%, CRIT %.0f%%", interval, warnPct, critPct)

	time.Sleep(20 * time.Second)
	for {
		for _, d := range getDisks() {
			pctStr, _ := d["percent"].(string)
			pctStr = strings.TrimSuffix(pctStr, "%")
			pct, err := strconv.ParseFloat(pctStr, 64)
			if err != nil {
				continue
			}
			mount, _ := d["mount"].(string)

			if pct >= critPct {
				key := "disk:critical:" + mount
				set, _ := rdb.SetNX(ctx, key, "1", 1*time.Hour).Result()
				if set {
					msg := fmt.Sprintf("🔴 %s — %v из %v (%.0f%%)", mount, d["used"], d["total"], pct)
					_ = rdb.Publish(ctx, "disk.critical", msg).Err()
					log.Printf("🔴 Диск %s заполнен на %.0f%% (critical)", mount, pct)
				}
			} else if pct >= warnPct {
				key := "disk:warning:" + mount
				set, _ := rdb.SetNX(ctx, key, "1", 6*time.Hour).Result()
				if set {
					msg := fmt.Sprintf("⚠️ %s — %v из %v (%.0f%%)", mount, d["used"], d["total"], pct)
					_ = rdb.Publish(ctx, "disk.warning", msg).Err()
					log.Printf("⚠️ Диск %s заполнен на %.0f%%", mount, pct)
				}
			}
		}
		time.Sleep(time.Duration(interval) * time.Minute)
	}
}

// hostAgentURL — адрес хост-агента (sensors/SMART/cleanup).
func hostAgentURL() string {
	return strings.TrimRight(platform.GetEnv("HOST_AGENT_URL", "http://host.containers.internal:9998"), "/")
}

func hostAgentGet(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, hostAgentURL()+path, nil)
	if err != nil {
		return nil, err
	}
	if t := strings.TrimSpace(platform.GetEnv("ADMIN_TOKEN", "")); t != "" {
		req.Header.Set("X-Admin-Token", t)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("host-agent %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(resp.Body)
}

func proxyHostAgent(path string) fiber.Handler {
	return func(c fiber.Ctx) error {
		b, err := hostAgentGet(path)
		if err != nil {
			return c.Status(503).JSON(fiber.Map{"error": "host-agent unavailable"})
		}
		c.Set("Content-Type", "application/json")
		return c.Send(b)
	}
}
