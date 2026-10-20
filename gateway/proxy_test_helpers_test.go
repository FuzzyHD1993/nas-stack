package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// lockedString — потокобезопасная строка: upstream-handler пишет в неё из
// goroutine httptest-сервера, тест читает из своей goroutine.
type lockedString struct {
	mu sync.Mutex
	v  string
}

func (s *lockedString) Set(v string) {
	s.mu.Lock()
	s.v = v
	s.mu.Unlock()
}

func (s *lockedString) Get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

// upstreamRequest — снимок запроса, дошедшего до подставного upstream.
type upstreamRequest struct {
	Method string
	Path   string
	Query  string
	Host   string
	Header http.Header
	Body   []byte
}

// upstreamRecorder потокобезопасно накапливает запросы upstream.
type upstreamRecorder struct {
	mu    sync.Mutex
	calls int
	reqs  []upstreamRequest
}

func (u *upstreamRecorder) record(r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		// Возвращаем тело handler'у: recorder может стоять перед полезным handler'ом.
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.reqs = append(u.reqs, upstreamRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Host:   r.Host,
		Header: r.Header.Clone(),
		Body:   body,
	})
}

// snapshot возвращает число вызовов и копию записей.
func (u *upstreamRecorder) snapshot() (int, []upstreamRequest) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]upstreamRequest, len(u.reqs))
	copy(out, u.reqs)
	return u.calls, out
}

// last возвращает число вызовов и последний запрос.
// При нуле вызовов — t.Fatalf: «заголовка нет» не должно проходить вакуумно.
func (u *upstreamRecorder) last(t *testing.T) (int, upstreamRequest) {
	t.Helper()
	calls, reqs := u.snapshot()
	if len(reqs) == 0 {
		t.Fatalf("upstream: no requests recorded")
		return calls, upstreamRequest{}
	}
	return calls, reqs[len(reqs)-1]
}

func newRecordingUpstream(t *testing.T) (*upstreamRecorder, *httptest.Server) {
	t.Helper()
	return newRecordingUpstreamWithHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

func newRecordingUpstreamWithHandler(t *testing.T, h http.HandlerFunc) (*upstreamRecorder, *httptest.Server) {
	t.Helper()
	rec := &upstreamRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func doProxy(t *testing.T, app *fiber.App, method, target string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	h := make(http.Header, len(headers))
	for k, v := range headers {
		if http.CanonicalHeaderKey(k) == "Host" {
			h.Set("Host", v)
			continue
		}
		h.Set(k, v)
	}
	return doProxyH(t, app, method, target, body, h)
}

// doProxyH позволяет duplicate/raw-заголовки через http.Header.
func doProxyH(t *testing.T, app *fiber.App, method, target string, body []byte, headers http.Header) *http.Response {
	t.Helper()
	if target == "" || target[0] != '/' {
		t.Fatalf("doProxy: target must be origin-form, got %q", target)
	}

	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}

	var req *http.Request
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("invalid request %q %q: %v", method, target, p)
			}
		}()
		req = httptest.NewRequest(method, target, r)
	}()

	headers = headers.Clone()
	if hosts := headers.Values("Host"); len(hosts) > 0 {
		if len(hosts) != 1 {
			t.Fatalf("multiple Host headers: %v", hosts)
		}
		req.Host = hosts[0]
		headers.Del("Host")
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 15 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("%s %s: app.Test: %v", method, target, err)
	}
	if resp == nil {
		t.Fatalf("%s %s: app.Test returned nil response", method, target)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return data
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode == want {
		return
	}
	t.Fatalf("status = %d, want %d; body = %q", resp.StatusCode, want, string(readBody(t, resp)))
}

func upstreamHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse upstream URL %q: %v", rawURL, err)
	}
	return u.Host
}
