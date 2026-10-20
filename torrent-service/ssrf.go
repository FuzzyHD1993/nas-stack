package main

import (
	"context"
	"errors"
	"fmt"
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
)

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
	resp, err := safeHTTPClient().Do(req)
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

// rewriteHost заменяет только точное имя хоста, а не подстроку в path/query.
func rewriteHost(u *url.URL) string {
	hostIP := strings.TrimSpace(platform.GetEnv("HOST_IP", ""))
	if hostIP == "" || !strings.EqualFold(u.Hostname(), "host.containers.internal") {
		return u.String()
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(hostIP, port)
	} else {
		u.Host = hostIP
	}
	return u.String()
}
