package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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

// withResolver подменяет DNS на время теста и сбрасывает кэш.
func withResolver(t *testing.T, ips ...string) {
	t.Helper()

	parsed := make([]net.IP, 0, len(ips))
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("невалидный IP в тесте: %q", raw)
		}
		parsed = append(parsed, ip)
	}

	prev := lookupIPAddr
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		out := make([]net.IPAddr, 0, len(parsed))
		for _, ip := range parsed {
			out = append(out, net.IPAddr{IP: ip})
		}
		return out, nil
	}
	clearDNSCache()
	t.Cleanup(func() {
		lookupIPAddr = prev
		clearDNSCache()
	})
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
		"::ffff:10.0.0.1",  // IPv4-mapped private
		"::ffff:127.0.0.1", // IPv4-mapped loopback
		"::ffff:169.254.169.254",
		"2002:0a00:0001::", // 6to4 10.0.0.1
		"64:ff9b::a00:1",   // NAT64 10.0.0.1
	}
	for _, raw := range blocked {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("невалидный IP в тесте: %q", raw)
		}
		if !isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = false, ожидался true", raw)
		}
	}

	public := []string{
		"93.184.216.34", "8.8.8.8",
		"2606:2800:220:1:248:1893:25c8:1946",
		"2002:5db8:d822::", // 6to4 93.184.216.34
		"64:ff9b::5db8:d822",
		"100.63.255.255", "100.128.0.0", // границы CGNAT
	}
	for _, raw := range public {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("невалидный IP в тесте: %q", raw)
		}
		if isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = true, ожидался false", raw)
		}
	}

	if !isBlockedIP(nil) {
		t.Error("isBlockedIP(nil) = false, ожидался true")
	}
	if !isBlockedIP(net.IP{}) {
		t.Error("isBlockedIP(net.IP{}) = false, ожидался true (fail-closed)")
	}
}

func TestIsSafeURLBlocksInternalTargets(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is,https://flibusta.net,https://flibusta.lib")
	t.Setenv("FLIBUSTA_LOCAL_URL", "http://host.containers.internal:27100")
	t.Setenv("FLIBUSTA_URL", "https://flibusta.is")
	withResolver(t, "93.184.216.34")

	bad := []string{
		"http://localhost:8080/health",
		"http://localhost/",
		"http://LOCALHOST./",
		"http://foo.localhost/health",
		"http://127.0.0.1:8087/health",
		"http://127.1/",
		"http://2130706433/",
		"http://0177.0.0.1/",
		"http://[::1]:8087/health",
		"http://[::ffff:127.0.0.1]/",
		"http://[::ffff:169.254.169.254]/",
		"http://[64:ff9b::a9fe:a9fe]/",
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://10.0.0.5:8080/status",
		"http://172.17.0.1:9998/update/run",
		"http://192.168.0.4:9998/cleanup/run",
		"http://gateway:8080/health",
		"http://redis:6379/",
		"http://evil.example.com/x",
		"https://flibusta.is.evil.example/",
		"https://evilflibusta.is/",
		"https://flibusta.is:22/x",
		"http://flibusta.is@169.254.169.254/",
		"http://169.254.169.254.nip.io/",
		"http://100.64.0.1/",
		"//169.254.169.254/",
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
	t.Setenv("FLIBUSTA_LOCAL_URL", "http://host.containers.internal:27100")
	t.Setenv("FLIBUSTA_URL", "https://flibusta.is")
	withResolver(t, "93.184.216.34")

	good := []string{
		"http://host.containers.internal:27100/fb2.php?id=6013",
		"https://flibusta.is/opds",
	}
	for _, raw := range good {
		if !isSafeURL(raw) {
			t.Errorf("isSafeURL(%q) = false, ожидался true", raw)
		}
	}

	// Порт — часть политики: локальное зеркало не открывает все порты хоста.
	if isSafeURL("http://host.containers.internal:27101/fb2.php?id=1") {
		t.Error("другой порт локального зеркала не должен быть разрешён")
	}
}

func TestIsSafeURLRejectsAllowlistedDomainResolvingPrivate(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://mirror.example")
	t.Setenv("FLIBUSTA_LOCAL_URL", "")

	withResolver(t, "192.168.0.1")
	if isSafeURL("https://cdn.flibusta.net/opds") {
		t.Fatal("allowlisted-домен, резолвящийся в RFC1918, должен быть отвергнут")
	}

	withResolver(t, "93.184.216.34")
	if !isSafeURL("https://cdn.flibusta.net/opds") {
		t.Fatal("публичный резолв должен проходить")
	}
}

func TestHostResolvesPublicCachesVerdict(t *testing.T) {
	var calls atomic.Int32

	prev := lookupIPAddr
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		calls.Add(1)
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}
	clearDNSCache()
	t.Cleanup(func() { lookupIPAddr = prev; clearDNSCache() })

	if !hostResolvesPublic("cache.example") {
		t.Fatal("публичный адрес должен считаться публичным")
	}
	if !hostResolvesPublic("cache.example") {
		t.Fatal("повторный вызов должен брать значение из кэша")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver calls = %d, want 1", got)
	}
}

func TestHostResolvesPublicCachesNegative(t *testing.T) {
	var calls atomic.Int32

	prev := lookupIPAddr
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		calls.Add(1)
		return nil, errors.New("nxdomain")
	}
	clearDNSCache()
	t.Cleanup(func() { lookupIPAddr = prev; clearDNSCache() })

	if hostResolvesPublic("err.example") {
		t.Fatal("ошибка DNS должна давать отказ")
	}
	if hostResolvesPublic("err.example") {
		t.Fatal("ошибка DNS должна давать отказ")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver calls = %d, want 1 (negative cache не работает)", got)
	}
}

func TestPinnedDialRejectsReboundAddress(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.net")
	withResolver(t, "169.254.169.254")

	dial := pinnedDialContext(&net.Dialer{Timeout: time.Second})
	if _, err := dial(context.Background(), "tcp", "cdn.flibusta.net:443"); err == nil {
		t.Fatal("dial на link-local после смены DNS не заблокирован")
	}
}

// /cover: проверяется настоящий обработчик из newApp(), а не копия.
func TestCoverRejectsSSRF(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is")
	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	withResolver(t, "93.184.216.34")

	var hit atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	app := newApp()

	for _, raw := range []string{
		upstream.URL + "/img.jpg",
		"http://gateway:8080/health",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.0.4:9998/cleanup/run",
		"file:///etc/passwd",
	} {
		resp, err := app.Test(
			httptest.NewRequest(http.MethodGet, "/cover?url="+url.QueryEscape(raw), nil),
			fiber.TestConfig{Timeout: 5 * time.Second},
		)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("/cover?url=%s → %d, ожидался 400", raw, resp.StatusCode)
		}
		if strings.Contains(string(body), raw) {
			t.Errorf("ответ отражает входной URL: %s", body)
		}
	}
	if hit.Load() {
		t.Fatal("запрос к внутреннему хосту всё-таки ушёл в сеть")
	}
}

func TestEmptyCoverURL(t *testing.T) {
	app := newApp()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/cover", nil),
		fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("пустой url: статус %d, ожидался 400", resp.StatusCode)
	}
}

// Политика редиректов проверяется офлайн: positive control + лимит цепочки.
func TestClientCheckRedirectPolicy(t *testing.T) {
	t.Setenv("FLIBUSTA_MIRRORS", "https://flibusta.is")
	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	withResolver(t, "93.184.216.34")

	check := makeClient().CheckRedirect
	allowed, err := newRequest(http.MethodGet, "https://flibusta.is/opds")
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	if err := check(allowed, []*http.Request{allowed}); err != nil {
		t.Fatalf("редирект на allowlist-хост заблокирован: %v", err)
	}

	blocked, _ := newRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/")
	if err := check(blocked, []*http.Request{blocked}); !errors.Is(err, errRedirectBlocked) {
		t.Fatalf("редирект на link-local не заблокирован: %v", err)
	}

	if err := check(allowed, make([]*http.Request, 5)); !errors.Is(err, errTooManyRedirects) {
		t.Fatalf("лимит цепочки редиректов не сработал: %v", err)
	}
}

// Редирект во внутреннюю сеть режется на уровне http.Client.
func TestClientCheckRedirectBlocksInternal(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	resetHTTPClientForTest(t)

	var hits atomic.Int32
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(start.Close)
	startURL := "http://" + start.Listener.Addr().String()

	// Стартовый хост — из конфига оператора, иначе pinnedDialContext его
	// не выпустит (сам isSafeURL loopback-литерал по-прежнему запрещает).
	t.Setenv("FLIBUSTA_MIRRORS", startURL)
	t.Setenv("FLIBUSTA_LOCAL_URL", "")
	clearDNSCache()

	hc := makeClient()
	req, err := newRequest(http.MethodGet, startURL+"/cover.jpg")
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}

	resp, err := hc.Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("редирект во внутреннюю сеть был выполнен")
	}
	if !errors.Is(err, errRedirectBlocked) {
		t.Fatalf("ожидалась errRedirectBlocked, получено: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("стартовый хост дёрнут %d раз, ожидался 1", n)
	}
}
