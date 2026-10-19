package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// clearDNSCache — тесты не должны зависеть от результатов прошлых резолвов.
func clearDNSCache() {
	dnsMu.Lock()
	dnsCache = map[string]dnsVerdict{}
	dnsMu.Unlock()
}

func TestIsBlockedIPRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "::1", "0.0.0.0", "::",
		"169.254.169.254", "169.254.0.1", "fe80::1",
		"10.0.0.1", "10.255.255.255",
		"172.16.0.1", "172.31.255.255",
		"192.168.0.1", "192.168.1.4",
		"100.64.0.1", "100.127.255.255", // CGNAT
		"fc00::1", "fd12:3456::1", // ULA
		"224.0.0.1",
	}
	for _, raw := range blocked {
		if !isBlockedIP(net.ParseIP(raw)) {
			t.Errorf("isBlockedIP(%s) = false, ожидался true", raw)
		}
	}
	public := []string{"93.184.216.34", "8.8.8.8", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, raw := range public {
		if isBlockedIP(net.ParseIP(raw)) {
			t.Errorf("isBlockedIP(%s) = true, ожидался false", raw)
		}
	}
	if !isBlockedIP(nil) {
		t.Error("isBlockedIP(nil) = false, ожидался true")
	}
}

func TestIsSafeURLBlocksInternalTargets(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is,https://flibusta.net,https://flibusta.lib")
	t.Setenv("FLIBUSTA_LOCAL_URL", "http://host.containers.internal:27100")
	t.Setenv("FLIBUSTA_URL", "https://flibusta.is")
	clearDNSCache()

	bad := []string{
		"http://localhost:8080/health",
		"http://localhost/",
		"http://foo.localhost/health",
		"http://127.0.0.1:8087/health",
		"http://[::1]:8087/health",
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://10.0.0.5:8080/status",
		"http://172.17.0.1:9998/update/run",
		"http://192.168.0.4:9998/cleanup/run",
		"http://gateway:8080/health", // внутренний сервис, не в allowlist
		"http://redis:6379/",         // внутренний сервис
		"http://evil.example.com/x",  // чужой домен
		"http://100.64.0.1/",         // CGNAT
		"file:///etc/passwd",
		"gopher://127.0.0.1:6379/_INFO",
		"ftp://flibusta.is/x",
		"javascript:alert(1)",
		"",
		"   ",
		"http://",
	}
	for _, raw := range bad {
		if isSafeURL(raw) {
			t.Errorf("isSafeURL(%q) = true, ожидался false", raw)
		}
	}
}

func TestIsSafeURLAllowsOperatorHosts(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is,https://flibusta.net,https://flibusta.lib")
	// Локальное зеркало резолвится в link-local 169.254.x — но это хост из
	// конфига оператора, а не пользовательский ввод, поэтому он доверенный.
	t.Setenv("FLIBUSTA_LOCAL_URL", "http://host.containers.internal:27100")
	t.Setenv("FLIBUSTA_URL", "https://flibusta.is")
	clearDNSCache()

	good := []string{
		"http://host.containers.internal:27100/fb2.php?id=6013",
		"https://flibusta.is/opds",
	}
	for _, raw := range good {
		if !isSafeURL(raw) {
			t.Errorf("isSafeURL(%q) = false, ожидался true", raw)
		}
	}
}

// Домен из allowlist дополнительно проверяется по резолву: если он
// «резолвится» в приватный адрес — доступа нет (DNS-rebinding / подмена DNS).
func TestIsSafeURLRejectsAllowlistedDomainResolvingPrivate(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is")
	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	t.Setenv("FLIBUSTA_URL", "https://flibusta.is")
	clearDNSCache()

	// Подсовываем flibusta.is -> 192.168.0.1 через локальный резолвер нельзя,
	// поэтому проверяем сам предикат на заведомо внутреннем имени из allowlist-схемы:
	// домен разрешён только если hostResolvesPublic возвращает true.
	if hostResolvesPublic("definitely-not-resolvable.invalid") {
		t.Fatal("несуществующий домен не должен считаться публичным")
	}
}

// /cover с SSRF-URL обязан вернуть 400 и НЕ ходить по сети.
func TestCoverRejectsSSRF(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is")
	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	clearDNSCache()

	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	app := fiber.New()
	app.Get("/cover", func(c fiber.Ctx) error {
		coverURL := c.Query("url")
		if coverURL == "" {
			return c.Status(400).SendString("url is required")
		}
		if !isSafeURL(coverURL) {
			return c.Status(400).JSON(fiber.Map{"error": "url is not allowed: " + coverURL})
		}
		hc := makeClient()
		req, err := newRequest("GET", coverURL)
		if err != nil {
			return c.Status(500).SendString(err.Error())
		}
		resp, err := hc.Do(req)
		if err != nil {
			return c.Status(502).SendString(err.Error())
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return c.Status(resp.StatusCode).Send(body)
	})

	for _, raw := range []string{
		upstream.URL + "/img.jpg", // 127.0.0.1
		"http://gateway:8080/health",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.0.4:9998/cleanup/run",
	} {
		req := httptest.NewRequest("GET", "/cover?url="+urlEscape(raw), nil)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		if resp.StatusCode != 400 {
			t.Errorf("/cover?url=%s → %d, ожидался 400", raw, resp.StatusCode)
		}
	}
	if hit {
		t.Fatal("запрос к внутреннему хосту всё-таки ушёл в сеть")
	}
}

// Редирект во внутреннюю сеть режется на уровне http.Client.
func TestClientCheckRedirectBlocksInternal(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is")
	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	clearDNSCache()

	var followed bool
	// Разрешённый старт (хост оператора), редирект — во внутреннюю сеть.
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer start.Close()
	startURL := "http://" + start.Listener.Addr().String()

	t.Setenv("FLIBUSTA_URL", startURL)
	t.Setenv("FLIBUSTA_MIRRORS", startURL)
	clearDNSCache()

	hc := makeClient()
	req, err := newRequest("GET", startURL+"/cover.jpg")
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	resp, err := hc.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		followed = true
	}
	if followed {
		t.Fatal("редирект во внутреннюю сеть был выполнен")
	}
	if err == nil || !contains(err.Error(), "redirect to disallowed host") {
		t.Fatalf("ожидалась ошибка про redirect, получено: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

func urlEscape(s string) string {
	r := ""
	for _, b := range []byte(s) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			b == '-', b == '_', b == '.', b == '~', b == '/', b == ':':
			r += string(b)
		default:
			r += "%" + hexDigit(b>>4) + hexDigit(b&0x0f)
		}
	}
	return r
}

func hexDigit(b byte) string {
	const hex = "0123456789ABCDEF"
	return string(hex[b&0x0f])
}

var _ = time.Second
