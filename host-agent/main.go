package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// newApp собирает приложение агента. Вынесено из main(), потому что на каждый
// адрес прослушивания нужен СВОЙ экземпляр: fiber не умеет обслуживать один
// App двумя слушателями (app.Listener + app.Listen ломают startupProcess).
func newApp() *fiber.App {
	app := fiber.New()

	app.Get("/metrics", adaptor.HTTPHandler(promhttp.Handler()))

	app.Get("/health", func(c fiber.Ctx) error {
		host, _ := os.Hostname()
		kernel := kernelVersion()
		return c.JSON(fiber.Map{"status": "ok", "service": "host-agent", "hostname": host, "kernel": kernel, "time": time.Now()})
	})

	app.Get("/sensors", func(c fiber.Ctx) error {
		out, err := exec.Command("sensors", "-j").Output()
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "sensors: " + err.Error()})
		}
		return c.JSON(parseSensors(out))
	})

	app.Get("/smart", func(c fiber.Ctx) error {
		return c.JSON(getSmart())
	})

	app.Get("/disks", func(c fiber.Ctx) error {
		return c.JSON(getDisks())
	})

	app.Get("/processes", func(c fiber.Ctx) error {
		limit, _ := strconv.Atoi(c.Query("limit", "10"))
		if limit <= 0 || limit > 100 {
			limit = 10
		}
		return c.JSON(getProcesses(limit))
	})

	app.Get("/network", func(c fiber.Ctx) error {
		iface := c.Query("iface", "eno1")
		return c.JSON(getNetwork(iface))
	})

	app.Get("/uptime", func(c fiber.Ctx) error {
		out, _ := exec.Command("uptime", "-p").Output()
		return c.JSON(fiber.Map{"uptime": strings.TrimSpace(string(out))})
	})

	app.Get("/load", func(c fiber.Ctx) error {
		data, _ := os.ReadFile("/proc/loadavg")
		fields := strings.Fields(string(data))
		return c.JSON(fiber.Map{"raw": strings.TrimSpace(string(data)), "load1": atof(fields, 0), "load5": atof(fields, 1), "load15": atof(fields, 2)})
	})

	app.Get("/gpu", func(c fiber.Ctx) error {
		out, err := exec.Command("nvidia-smi",
			"--query-gpu=name,temperature.gpu,utilization.gpu,memory.used,memory.total",
			"--format=csv,noheader,nounits").Output()
		if err != nil {
			return c.JSON(fiber.Map{"error": "nvidia-smi not available"})
		}
		parts := strings.Split(strings.TrimSpace(string(out)), ", ")
		if len(parts) < 5 {
			return c.JSON(fiber.Map{"error": "unexpected nvidia-smi output"})
		}
		return c.JSON(fiber.Map{
			"name": parts[0], "temp": parts[1] + "°C", "usage": parts[2] + "%",
			"memory_used": parts[3] + " MB", "memory_total": parts[4] + " MB",
		})
	})

	app.Get("/cleanup/preview", func(c fiber.Ctx) error {
		return c.JSON(cleanupPreview())
	})

	app.Post("/cleanup/run", requireAdminToken, func(c fiber.Ctx) error {
		dry := c.Query("dry", "true") != "false"
		var body struct {
			Targets      []string `json:"targets"`
			AllowKernels bool     `json:"allow_kernels"`
		}
		_ = c.Bind().JSON(&body)
		if dry {
			return c.JSON(fiber.Map{"dry": true, "preview": cleanupPreview()})
		}
		return c.JSON(runCleanup(body.Targets, body.AllowKernels))
	})

	// Перемещение дубликатов в бэкап (медиа смонтировано ro — пишем через агент).
	// ---- Auto-update (git + compose на хосте) ----
	repoDir := func() string {
		return platform.GetEnv("REPO_DIR", "/home/konstantin/projects/nas-stack")
	}
	app.Get("/update/status", func(c fiber.Ctx) error {
		dir := repoDir()
		_ = exec.Command("git", "-C", dir, "fetch", "--quiet", "github", "main").Run()
		cur, _ := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
		cnt, _ := exec.Command("git", "-C", dir, "rev-list", "HEAD..github/main", "--count").Output()
		n, _ := strconv.Atoi(strings.TrimSpace(string(cnt)))
		return c.JSON(fiber.Map{
			"current_commit":    strings.TrimSpace(string(cur)),
			"updates_available": n,
		})
	})
	app.Post("/update/run", requireAdminToken, func(c fiber.Ctx) error {
		dir := repoDir()
		go func() {
			cmd := exec.Command("bash", dir+"/scripts/update.sh")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			log.Printf("🔄 update: err=%v\n%s", err, out)
		}()
		return c.JSON(fiber.Map{"status": "started"})
	})

	app.Post("/duplicates/resolve", requireAdminToken, func(c fiber.Ctx) error {
		var req struct {
			Paths []string `json:"paths"`
			Keep  string   `json:"keep"`
		}
		if err := c.Bind().JSON(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "bad json"})
		}
		const root = "/mnt/nas/root/FuzzyHD/"
		libraryRoot := platform.GetEnv("LIBRARY_ROOT", root)
		backupDir := platform.GetEnv("DUPLICATES_BACKUP_DIR", "/mnt/nas/Backups/duplicates")
		if !strings.HasSuffix(libraryRoot, "/") {
			libraryRoot += "/"
		}
		if err := os.MkdirAll(backupDir, 0o755); err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		moved, skipped := []string{}, []string{}
		for _, p := range req.Paths {
			if p == req.Keep || !strings.HasPrefix(p, libraryRoot) {
				skipped = append(skipped, p)
				continue
			}
			target := filepathJoin(backupDir, filepathBase(p))
			if _, err := os.Stat(target); err == nil {
				ext := filepathExt(target)
				target = strings.TrimSuffix(target, ext) + "-" + strconv.FormatInt(time.Now().Unix(), 10) + ext
			}
			if err := os.Rename(p, target); err != nil {
				log.Printf("⚠️ rename %s → %s: %v", p, target, err)
				skipped = append(skipped, p)
				continue
			}
			moved = append(moved, p)
		}
		return c.JSON(fiber.Map{"status": "ok", "moved": moved, "skipped": skipped})
	})

	return app
}

// main поднимает агент на адресах из HOST_AGENT_BIND.
//
// P0: раньше было жёстко app.Listen("0.0.0.0:9998") — слушали все интерфейсы,
// и любой в LAN дёргал /update/run, /cleanup/run, /duplicates/resolve.
// Теперь по умолчанию ТОЛЬКО loopback (127.0.0.1:9998). Контейнеры стека
// ходят на агента через host.containers.internal, который резолвится не в
// loopback, а в адрес хоста, поэтому дополнительный адрес задаётся в .env:
//
//	HOST_AGENT_BIND=127.0.0.1:9998,192.168.0.4:9998
//
// Сам host-exec дополнительно закрыт ADMIN_TOKEN (см. requireAdminToken),
// так что даже при 0.0.0.0 в bind опасные ручки недоступны без токена.
func main() {
	binds := agentBindAddrs()

	if adminToken() == "" {
		log.Println("⚠️ ADMIN_TOKEN не задан — /update/run, /cleanup/run и /duplicates/resolve будут отдавать 403")
	}

	for _, extra := range binds[1:] {
		addr := extra
		go func() {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				log.Printf("⚠️ host-agent: не открыт слушатель %s: %v", addr, err)
				return
			}
			log.Printf("🖥️  host-agent дополнительный слушатель: %s", addr)
			if err := newApp().Listener(ln); err != nil {
				log.Printf("⚠️ host-agent слушатель %s остановлен: %v", addr, err)
			}
		}()
	}

	log.Printf("🖥️  host-agent на %s", binds[0])
	log.Fatal(newApp().Listen(binds[0]))
}

// stackEnvValue — platform.GetEnv с fallback'ом на REPO_DIR/.env.
// Нативные юниты (systemd/dinit) не наследуют содержимое .env стека, а
// ADMIN_TOKEN и HOST_AGENT_BIND хранятся именно там (паттерн gatewayToken
// в notify-service).
func stackEnvValue(key, def string) string {
	if v := strings.TrimSpace(platform.GetEnv(key, "")); v != "" {
		return v
	}
	repoDir := platform.GetEnv("REPO_DIR", "/home/konstantin/projects/nas-stack")
	data, err := os.ReadFile(filepathJoin(repoDir, ".env"))
	if err != nil {
		return def
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return def
}

// adminToken — секрет админ-операций (тот же ADMIN_TOKEN, что на gateway
// и scheduler).
func adminToken() string {
	return stackEnvValue("ADMIN_TOKEN", "")
}

// agentBindAddrs — список адресов прослушивания из HOST_AGENT_BIND
// (через запятую). По умолчанию только loopback.
func agentBindAddrs() []string {
	var out []string
	raw := stackEnvValue("HOST_AGENT_BIND", "127.0.0.1:9998")
	for _, a := range strings.Split(raw, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		out = []string{"127.0.0.1:9998"}
	}
	return out
}

// adminTokenOK — сверяет X-Admin-Token или Authorization: Bearer с ADMIN_TOKEN
// постоянным временем сравнения. Пустой ADMIN_TOKEN = fail-closed.
func adminTokenOK(c fiber.Ctx) bool {
	expected := adminToken()
	if expected == "" {
		return false
	}
	got := strings.TrimSpace(c.Get("X-Admin-Token"))
	if got == "" {
		if auth := c.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// requireAdminToken — миддлварь для ручек с побочными эффектами на хосте:
// /update/run (git+compose), /cleanup/run (podman prune, rm -rf trash),
// /duplicates/resolve (перемещение файлов библиотеки).
func requireAdminToken(c fiber.Ctx) error {
	if adminTokenOK(c) {
		return c.Next()
	}
	return c.Status(403).JSON(fiber.Map{"error": "forbidden: admin token required"})
}

func kernelVersion() string {
	out, _ := exec.Command("uname", "-r").Output()
	return strings.TrimSpace(string(out))
}

func atof(f []string, i int) float64 {
	if i < len(f) {
		v, _ := strconv.ParseFloat(f[i], 64)
		return v
	}
	return 0
}

// ============================== SENSORS ==============================

// parseSensors разбирает JSON от `sensors -j` и извлекает температуры.
func parseSensors(data []byte) fiber.Map {
	var root map[string]map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return fiber.Map{"error": "parse: " + err.Error()}
	}

	type temp struct {
		Label string  `json:"label"`
		Temp  float64 `json:"temp"`
		Max   float64 `json:"max,omitempty"`
		Crit  float64 `json:"crit,omitempty"`
	}

	cpuPackage := fiber.Map{}
	cores := []temp{}
	nvme := []temp{}
	other := []temp{}

	tempRe := regexp.MustCompile(`(?i)temp|package|composite|core|tctl|tccd|sensor`)
	coreRe := regexp.MustCompile(`(?i)\bcore\b|tccd`)

	for chip, features := range root {
		chipLow := strings.ToLower(chip)
		for feat, raw := range features {
			if !tempRe.MatchString(feat) {
				continue
			}
			subs, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			var input, max, crit float64
			hasInput := false
			for sub, val := range subs {
				f, ok := toFloat(val)
				if !ok {
					continue
				}
				ls := strings.ToLower(sub)
				switch {
				case strings.HasSuffix(ls, "input"):
					input = f
					hasInput = true
				case strings.HasSuffix(ls, "max"), strings.HasSuffix(ls, "high"):
					if f > max {
						max = f
					}
				case strings.HasSuffix(ls, "crit"):
					crit = f
				}
			}
			if !hasInput {
				continue
			}
			t := temp{Label: feat, Temp: input, Max: max, Crit: crit}
			switch {
			case strings.Contains(chipLow, "nvme"):
				nvme = append(nvme, t)
			case strings.Contains(strings.ToLower(feat), "package"):
				if _, ok := cpuPackage["temp"]; !ok {
					cpuPackage = fiber.Map{"label": feat, "temp": input, "max": max}
				}
			case coreRe.MatchString(feat):
				cores = append(cores, t)
			default:
				other = append(other, t)
			}
		}
	}

	sort.SliceStable(cores, func(i, j int) bool { return cores[i].Label < cores[j].Label })
	return fiber.Map{
		"cpu_package": cpuPackage,
		"cores":       cores,
		"nvme":        nvme,
		"other":       other,
	}
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// ============================== SMART ==============================

func getSmart() []fiber.Map {
	candidates := []string{"/dev/nvme0", "/dev/nvme0n1", "/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"}
	result := []fiber.Map{}
	for _, dev := range candidates {
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		result = append(result, smartOne(dev))
	}
	return result
}

func smartOne(dev string) fiber.Map {
	// smartctl требует root — пробуем через passwordless sudo.
	out, err := exec.Command("sudo", "-n", "smartctl", "-a", "-j", dev).Output()
	if err != nil && len(out) == 0 {
		out, err = exec.Command("smartctl", "-a", "-j", dev).Output()
	}
	if len(out) == 0 {
		msg := "smartctl failed"
		if err != nil {
			msg = err.Error()
		}
		return fiber.Map{"device": dev, "error": msg}
	}
	var d map[string]interface{}
	if json.Unmarshal(out, &d) != nil {
		return fiber.Map{"device": dev, "error": "parse error"}
	}
	// Проверяем сообщения smartctl (напр. Permission denied).
	if sm, ok := d["smartctl"].(map[string]interface{}); ok {
		if msgs, ok := sm["messages"].([]interface{}); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]interface{}); ok {
					if sev, _ := mm["severity"].(string); sev == "error" {
						s, _ := mm["string"].(string)
						return fiber.Map{"device": dev, "error": s}
					}
				}
			}
		}
	}

	status := "UNKNOWN"
	if ss, ok := d["smart_status"].(map[string]interface{}); ok {
		if passed, ok := ss["passed"].(bool); ok {
			if passed {
				status = "PASSED"
			} else {
				status = "FAILED"
			}
		}
	}

	temp := 0
	if t, ok := d["temperature"].(map[string]interface{}); ok {
		temp = intAt(t, "current")
	}
	powerOn := intAt(d, "power_on_time")
	if poh, ok := d["power_on_time"].(map[string]interface{}); ok {
		powerOn = intAt(poh, "hours")
	}

	wear := -1
	if nv, ok := d["nvme_smart_health_information_log"].(map[string]interface{}); ok {
		wear = intAt(nv, "percentage_used")
		if t := intAt(nv, "temperature"); t > 0 {
			temp = t
		}
		if h := intAt(nv, "power_on_hours"); h > 0 {
			powerOn = h
		}
	}

	model, _ := d["model_name"].(string)
	serial, _ := d["serial_number"].(string)
	proto, _ := d["device"].(map[string]interface{})
	protocol := ""
	if proto != nil {
		protocol, _ = proto["protocol"].(string)
	}

	res := fiber.Map{
		"device":         dev,
		"model":          model,
		"serial":         serial,
		"protocol":       protocol,
		"health":         status,
		"smart_status":   status == "PASSED",
		"temperature":    temp,
		"power_on_hours": powerOn,
	}
	if wear >= 0 {
		res["wear_level"] = wear
	}
	return res
}

func intAt(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	if v, ok := m[key]; ok {
		if f, ok := toFloat(v); ok {
			return int(f)
		}
	}
	return 0
}

// ============================== DISKS ==============================

var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "btrfs": true, "xfs": true,
	"vfat": true, "exfat": true, "ntfs": true, "cifs": true, "nfs": true, "nfs4": true, "zfs": true,
}

func getDisks() []fiber.Map {
	out, _ := exec.Command("df", "-h", "--output=source,fstype,size,used,avail,pcent,target").Output()
	type disk struct{ source, fstype, total, used, avail, percent, mount string }
	seen := map[string]int{}
	list := []disk{}
	for _, line := range strings.Split(string(out), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 7 || !realFS[f[1]] {
			continue
		}
		d := disk{f[0], f[1], f[2], f[3], f[4], f[5], f[6]}
		if idx, ok := seen[f[0]]; ok {
			if len(d.mount) < len(list[idx].mount) {
				list[idx] = d
			}
			continue
		}
		seen[f[0]] = len(list)
		list = append(list, d)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].mount < list[j].mount })
	result := []fiber.Map{}
	for _, d := range list {
		result = append(result, fiber.Map{
			"source": d.source, "fstype": d.fstype, "total": d.total,
			"used": d.used, "avail": d.avail, "percent": d.percent, "mount": d.mount,
		})
	}
	return result
}

// ============================== PROCESSES ==============================

func getProcesses(limit int) []fiber.Map {
	out, _ := exec.Command("ps", "aux", "--sort=-%cpu").Output()
	result := []fiber.Map{}
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 11 {
			continue
		}
		name := strings.Join(f[10:], " ")
		if strings.HasPrefix(name, "ps ") {
			continue
		}
		result = append(result, fiber.Map{"user": f[0], "pid": f[1], "cpu": f[2], "mem": f[3], "name": name})
		if len(result) >= limit {
			break
		}
	}
	return result
}

// ============================== NETWORK ==============================

func getNetwork(iface string) fiber.Map {
	out, err := exec.Command("ip", "-s", "-j", "link", "show", iface).Output()
	if err != nil {
		return fiber.Map{"error": err.Error()}
	}
	var arr []map[string]interface{}
	if json.Unmarshal(out, &arr) != nil || len(arr) == 0 {
		return fiber.Map{"error": "parse error"}
	}
	obj := arr[0]
	stats, _ := obj["stats64"].(map[string]interface{})
	if stats == nil {
		stats, _ = obj["stats"].(map[string]interface{})
	}
	rx, _ := stats["rx"].(map[string]interface{})
	tx, _ := stats["tx"].(map[string]interface{})
	return fiber.Map{
		"iface":      iface,
		"rx_bytes":   intAt(rx, "bytes"),
		"tx_bytes":   intAt(tx, "bytes"),
		"rx_errors":  intAt(rx, "errors"),
		"tx_errors":  intAt(tx, "errors"),
		"rx_dropped": intAt(rx, "dropped"),
		"tx_dropped": intAt(tx, "dropped"),
		"rx_packets": intAt(rx, "packets"),
		"tx_packets": intAt(tx, "packets"),
	}
}

// ============================== CLEANUP ==============================

func cleanupPreview() []fiber.Map {
	items := []fiber.Map{}

	// podman
	if s := podmanReclaimable(); s > 0 {
		items = append(items, fiber.Map{"name": "podman_images", "size_bytes": s, "description": "Образы/контейнеры podman (prune)"})
	}
	// journald
	items = append(items, fiber.Map{"name": "journald", "size_bytes": dirSizeCmd("journalctl", "--disk-usage"), "description": "journald logs"})
	// cache
	home, _ := os.UserHomeDir()
	items = append(items, fiber.Map{"name": "cache", "size_bytes": duSize(filepathJoin(home, ".cache")), "description": "~/.cache"})
	// trash
	trash := duSize(filepathJoin(home, ".local/share/Trash"))
	items = append(items, fiber.Map{"name": "trash", "size_bytes": trash, "description": "Корзина"})
	// pacman
	items = append(items, fiber.Map{"name": "pacman_cache", "size_bytes": duSize("/var/cache/pacman/pkg"), "description": "pacman cache"})
	// old kernels (количество)
	items = append(items, fiber.Map{"name": "old_kernels", "size_bytes": 0, "count": oldKernels(), "description": "Старые ядра (не удаляются автоматически)"})

	return items
}

func runCleanup(targets []string, allowKernels bool) fiber.Map {
	want := map[string]bool{}
	for _, t := range targets {
		want[t] = true
	}
	doAll := len(targets) == 0
	results := []fiber.Map{}
	freed := int64(0)

	run := func(name, cmd string) {
		if !doAll && !want[name] {
			return
		}
		before := cleanupSizeFor(name)
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		if err != nil {
			results = append(results, fiber.Map{"target": name, "ok": false, "error": err.Error(), "output": strings.TrimSpace(string(out))})
			return
		}
		after := cleanupSizeFor(name)
		f := before - after
		if f < 0 {
			f = 0
		}
		freed += f
		results = append(results, fiber.Map{"target": name, "ok": true, "freed_bytes": f})
	}

	run("podman_images", "podman system prune -a --volumes -f")
	run("journald", "journalctl --vacuum-size=500M")
	run("cache", "find ~/.cache -type f -atime +30 -delete 2>/dev/null || true")
	run("trash", "rm -rf ~/.local/share/Trash/* 2>/dev/null || true")
	run("pacman_cache", "paccache -rk3")

	if allowKernels && (doAll || want["old_kernels"]) {
		results = append(results, fiber.Map{"target": "old_kernels", "ok": false, "error": "ручное удаление ядер не поддерживается"})
	}

	return fiber.Map{"dry": false, "freed_bytes": freed, "results": results}
}

func podmanReclaimable() int64 {
	out, err := exec.Command("podman", "system", "df", "--format", "json").Output()
	if err != nil || len(out) == 0 {
		return 0
	}
	var arr []map[string]interface{}
	if json.Unmarshal(out, &arr) != nil {
		return 0
	}
	var total int64
	for _, item := range arr {
		if v, ok := item["RawReclaimable"].(float64); ok {
			total += int64(v)
		}
	}
	return total
}

func cleanupSizeFor(name string) int64 {
	home, _ := os.UserHomeDir()
	switch name {
	case "podman_images":
		return podmanReclaimable()
	case "journald":
		return dirSizeCmd("journalctl", "--disk-usage")
	case "cache":
		return duSize(filepathJoin(home, ".cache"))
	case "trash":
		return duSize(filepathJoin(home, ".local/share/Trash"))
	case "pacman_cache":
		return duSize("/var/cache/pacman/pkg")
	}
	return 0
}

// dirSizeCmd — размер из команды вида "journalctl --disk-usage" (парсим "T archived").
func dirSizeCmd(name string, args ...string) int64 {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	// пример: "Archived and active journals take up 123.4M in the file system."
	re := regexp.MustCompile(`([0-9.,]+)\s*([KMGT]?B?)`)
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	return parseHumanSize(m[1] + m[2])
}

func duSize(path string) int64 {
	out, _ := exec.Command("du", "-sb", path).Output()
	f := strings.Fields(string(out))
	if len(f) < 1 {
		return 0
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	return n
}

func oldKernels() int {
	cur := kernelVersion()
	out, err := exec.Command("ls", "/usr/lib/modules").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, m := range strings.Fields(string(out)) {
		if m != cur {
			n++
		}
	}
	return n
}

// parseHumanSize переводит "1.5G"/"300M"/"12K" в байты.
func parseHumanSize(s string) int64 {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1024
		s = s[:len(s)-1]
	case 'M', 'm':
		mult = 1024 * 1024
		s = s[:len(s)-1]
	case 'G', 'g':
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case 'T', 't':
		mult = 1024 * 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case 'B', 'b':
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(mult))
}

func filepathJoin(a, b string) string {
	if strings.HasSuffix(a, "/") {
		return a + b
	}
	return a + "/" + b
}

func filepathBase(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return p
	}
	return p[i+1:]
}

func filepathExt(p string) string {
	b := filepathBase(p)
	i := strings.LastIndexByte(b, '.')
	if i <= 0 {
		return ""
	}
	return b[i:]
}
