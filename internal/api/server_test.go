package api

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

const indexHTML = "<!doctype html><div id=app></div>"

func newTestServer(t *testing.T, preferred int) (*Server, chan string) {
	t.Helper()
	opened := make(chan string, 8)
	s, err := New(t.Context(), Options{
		Assets: fstest.MapFS{
			"index.html":       {Data: []byte(indexHTML)},
			"assets/app-1.js":  {Data: []byte("console.log(1)")},
			"assets/app-1.css": {Data: []byte("body{}")},
		},
		PreferredPort: preferred,
		Version:       "test",
		Open:          func(u string) { opened <- u },
		Log:           slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	return s, opened
}

type result struct {
	status int
	path   string // final path after redirects
	body   string
}

func do(t *testing.T, c *http.Client, req *http.Request) result {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, path: resp.Request.URL.Path, body: string(body)}
}

func get(t *testing.T, c *http.Client, url, bearer string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return do(t, c, req)
}

func TestRandomPortAndPreferredFallback(t *testing.T) {
	s, _ := newTestServer(t, 0)
	if s.Port() == 0 {
		t.Fatal("port not assigned")
	}
	// Preferred port already taken by s → must fall back to another free port.
	s2, _ := newTestServer(t, s.Port())
	if s2.Port() == s.Port() || s2.Port() == 0 {
		t.Fatalf("fallback port = %d (busy %d)", s2.Port(), s.Port())
	}
	// Preferred port free → used.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("addr %v", ln.Addr())
	}
	_ = ln.Close()
	s3, _ := newTestServer(t, addr.Port)
	if s3.Port() != addr.Port {
		t.Fatalf("preferred free port %d not used, got %d", addr.Port, s3.Port())
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	s, _ := newTestServer(t, 0)
	for _, p := range []string{"/", "/item/1", "/api/health", "/assets/app-1.js"} {
		if r := get(t, http.DefaultClient, s.BaseURL()+p, ""); r.status != http.StatusUnauthorized {
			t.Errorf("%s without auth: status %d, want 401", p, r.status)
		}
	}
	if r := get(t, http.DefaultClient, s.BaseURL()+"/", "wrong"); r.status != http.StatusUnauthorized {
		t.Errorf("wrong bearer: status %d", r.status)
	}
}

func TestBearerServesSPAWithFallback(t *testing.T) {
	s, _ := newTestServer(t, 0)
	cases := map[string]string{
		"/":                indexHTML,
		"/item/42":         indexHTML, // client route → index
		"/assets/app-1.js": "console.log(1)",
	}
	for p, want := range cases {
		if r := get(t, http.DefaultClient, s.BaseURL()+p, s.Token()); r.status != http.StatusOK || r.body != want {
			t.Errorf("%s: status %d body %q, want %q", p, r.status, r.body, want)
		}
	}
	if r := get(t, http.DefaultClient, s.BaseURL()+"/assets/missing.js", s.Token()); r.status != http.StatusNotFound {
		t.Errorf("missing asset: status %d, want 404", r.status)
	}
	if r := get(t, http.DefaultClient, s.BaseURL()+"/api/health", s.Token()); r.status != http.StatusOK ||
		!strings.Contains(r.body, `"version":"test"`) {
		t.Errorf("health: %d %s", r.status, r.body)
	}
}

func TestLaunchURLSignsInOnce(t *testing.T) {
	s, _ := newTestServer(t, 0)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	launch := s.LaunchURL("/item/42")
	if r := get(t, c, launch, ""); r.status != http.StatusOK || r.body != indexHTML || r.path != "/item/42" {
		t.Fatalf("launch: status %d path %s body %q", r.status, r.path, r.body)
	}
	// Session cookie now authorizes the API.
	if r := get(t, c, s.BaseURL()+"/api/health", ""); r.status != http.StatusOK {
		t.Fatalf("cookie not accepted: %d", r.status)
	}
	// Token is single use.
	if r := get(t, &http.Client{}, launch, ""); r.status != http.StatusForbidden {
		t.Fatalf("reused launch token: status %d, want 403", r.status)
	}
}

func TestLaunchRejectsOffsiteRedirect(t *testing.T) {
	for _, next := range []string{"//evil.example/x", "https://evil.example/", `/\evil`, "item"} {
		if got := safeNext(next); got != "/" {
			t.Errorf("safeNext(%q) = %q, want /", next, got)
		}
	}
	if got := safeNext("/item/7?x=1"); got != "/item/7?x=1" {
		t.Errorf("safeNext kept path wrong: %q", got)
	}
}

func TestForeignHostRejected(t *testing.T) {
	s, _ := newTestServer(t, 0)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, s.BaseURL()+"/", nil)
	req.Host = "evil.example:" + strconv.Itoa(s.Port())
	req.Header.Set("Authorization", "Bearer "+s.Token())
	if r := do(t, http.DefaultClient, req); r.status != http.StatusForbidden {
		t.Fatalf("foreign host: status %d, want 403", r.status)
	}
}

func TestOpenEndpointOpensLaunchURL(t *testing.T) {
	s, opened := newTestServer(t, 0)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, s.BaseURL()+"/api/open",
		strings.NewReader(`{"path":"/item/5"}`))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	if r := do(t, http.DefaultClient, req); r.status != http.StatusNoContent {
		t.Fatalf("open: status %d", r.status)
	}
	if u := <-opened; !strings.HasPrefix(u, s.BaseURL()+"/auth?t=") {
		t.Fatalf("opened %q, want launch URL", u)
	}
}
