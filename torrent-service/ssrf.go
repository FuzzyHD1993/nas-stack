package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/FuzzyHD1993/nas-stack/internal/platform"
)

const (
	maxTorrentURLLen = 4096
	maxMagnetLen     = 4096
	maxRedirects     = 3
	// maxTorrentBodyBytes — лимит размера скачиваемого .torrent (10 MiB).
	maxTorrentBodyBytes = 10 << 20
)

// defaultDownloadBaseURL — согласован с DOWNLOAD_BASE_URL search-service
// (см. docker-compose.yml): базовый URL gateway, на котором лежит
// /search/download — внутренний download-прокси.
const defaultDownloadBaseURL = "http://host.containers.internal:8080"

// trustedDownloadPath — единственный путь, разрешённый trusted-allowlist'ом.
const trustedDownloadPath = "/search/download"

var (
	errBlockedAddr = errors.New("private/loopback address is not allowed")
	errBadScheme   = errors.New("only http and https urls are allowed")
)

// isBlockedIP — fail-closed: nil/непарсящийся IP тоже считается запрещённым.
func isBlockedIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

func validateMagnet(raw string) (string, error) {
	m := strings.TrimSpace(raw)
	if m == "" || len(m) > maxMagnetLen {
		return "", errors.New("invalid magnet length")
	}
	low := strings.ToLower(m)
	if !strings.HasPrefix(low, "magnet:?") {
		return "", errors.New("magnet scheme required")
	}
	if !strings.Contains(low, "xt=urn:btih:") && !strings.Contains(low, "xt=urn:btmh:") {
		return "", errors.New("magnet xt=urn:btih|btmh required")
	}
	return m, nil
}

func validateTorrentURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxTorrentURLLen {
		return nil, errors.New("invalid url length")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errBadScheme
	}
	if u.Host == "" || u.User != nil {
		return nil, errors.New("url host required, userinfo forbidden")
	}
	// Точечный allowlist для download-прокси нашего же gateway: такой URL
	// разрешён без SSRF-блокад (см. комментарий у trustedHTTPClient).
	if isTrustedDownloadURL(u) {
		u.Fragment = ""
		return u, nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, errBlockedAddr
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
		return nil, errBlockedAddr
	}
	u.Fragment = ""
	return u, nil
}

// safeHTTPClient блокирует приватные адреса на уровне dial (после DNS),
// поэтому DNS-rebinding и редиректы на 127.0.0.1/169.254.169.254 не проходят.
func safeHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if isBlockedIP(net.ParseIP(host)) {
				return fmt.Errorf("%w: %s", errBlockedAddr, host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil, // HTTP(S)_PROXY не должен уводить запрос во внутреннюю сеть
			DialContext:           dialer.DialContext,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return http.ErrUseLastResponse
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// downloadBaseURL возвращает базовый URL gateway из DOWNLOAD_BASE_URL
// (дефолт — host.containers.internal:8080), без хвостовых "/".
func downloadBaseURL() string {
	base := strings.TrimRight(strings.TrimSpace(platform.GetEnv("DOWNLOAD_BASE_URL", defaultDownloadBaseURL)), "/")
	if base == "" {
		return defaultDownloadBaseURL
	}
	return base
}

// isTrustedDownloadURL — allowlist нашего внутреннего download-прокси:
// URL считается доверенным ТОЛЬКО если схема и host:port совпадают
// с DOWNLOAD_BASE_URL, а путь — ровно /search/download.
func isTrustedDownloadURL(u *url.URL) bool {
	base, err := url.Parse(downloadBaseURL())
	if err != nil || base.Host == "" || base.Scheme == "" {
		return false
	}
	if !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
		return false
	}
	return u.EscapedPath() == trustedDownloadPath
}

// trustedHTTPClient используется только для allowlist'нутых download-URL.
//
// Это безопасно, потому что:
//   - allowlist жёстко ограничен: точное совпадение фиксированного хоста/порта
//     из DOWNLOAD_BASE_URL и единственного пути /search/download (никакие
//     другие хосты/пути/порты через него не проходят);
//   - gateway сам валидирует параметр url в /search/download (только пути
//     Prowlarr ".../download" + link), так что через этот эндпоинт нельзя
//     спросить произвольный внутренний ресурс.
//
// От safeHTTPClient отличается только отсутствием dial-контроля приватных
// адресов (иначе link-local host.containers.internal был бы заблокирован);
// редиректы не следуем никогда — resolveMagnet читает Location сам.
func trustedHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil, // HTTP(S)_PROXY не должен уводить запрос во внутреннюю сеть
			DialContext:           dialer.DialContext,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// resolveMagnet возвращает magnet, если URL отвечает 3xx с Location: magnet:...
func resolveMagnet(rctx context.Context, torrentURL string) string {
	pu, err := validateTorrentURL(torrentURL)
	if err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(rctx, 25*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pu.String(), nil)
	if err != nil {
		return ""
	}
	// Trusted-URL (наш download-прокси сети NAS) ходят без SSRF-блока,
	// остальные — как раньше.
	client := safeHTTPClient()
	if isTrustedDownloadURL(pu) {
		client = trustedHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	if m, err := validateMagnet(loc); err == nil {
		return m
	}
	return ""
}

// fetchTorrent скачивает .torrent-файл (не более maxTorrentBodyBytes) и возвращает
// только 200 OK; trusted-URL получает клиент без SSRF-блока, остальные — safe.
func fetchTorrent(rctx context.Context, rawURL string) ([]byte, error) {
	pu, err := validateTorrentURL(rawURL)
	if err != nil {
		return nil, err
	}
	client := safeHTTPClient()
	if isTrustedDownloadURL(pu) {
		client = trustedHTTPClient()
	}

	ctx, cancel := context.WithTimeout(rctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pu.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("torrent download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("torrent download: %w", err)
	}
	if len(data) > maxTorrentBodyBytes {
		return nil, fmt.Errorf("torrent download: body exceeds %d bytes", int64(maxTorrentBodyBytes))
	}
	return data, nil
}
