package websession

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
)

func TestJarRoundTripAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "nexus.json")
	j, err := Open(path)
	if err != nil || !j.Empty() {
		t.Fatalf("new jar: %v empty=%v", err, j.Empty())
	}
	now := time.Now()
	cks := FromBrowser([]browser.Cookie{
		{Name: "live", Value: "v1", Domain: ".example.com", Path: "/", Expires: float64(now.Add(time.Hour).Unix()), Secure: true},
		{Name: "gone", Value: "v2", Domain: ".example.com", Path: "/", Expires: float64(now.Add(-time.Hour).Unix())},
		{Name: "sess", Value: "v3", Domain: "www.example.com", Path: "/", Expires: -1, Session: true},
	})
	if err := j.Replace(cks, "UA/1", "Morgott", "window"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path) //nolint:gosec // G304: test temp file
	if strings.Contains(string(raw), "Morgott") || strings.Contains(string(raw), "example.com") {
		t.Fatal("jar file is not encrypted")
	}
	j2, err := Open(path)
	if err != nil || j2.UserAgent() != "UA/1" || j2.Account() != "Morgott" || j2.Source() != "window" {
		t.Fatalf("reopen: %v %q %q", err, j2.UserAgent(), j2.Account())
	}
	u, _ := url.Parse("https://www.example.com/x")
	if h := j2.Header(u); h != "live=v1; sess=v3" {
		t.Fatalf("header = %q (expired cookie must be dropped)", h)
	}
	sub, _ := url.Parse("https://api.example.com/")
	if h := j2.Header(sub); h != "live=v1" {
		t.Fatalf("subdomain header = %q (host-only cookie must stay on its host)", h)
	}
	plain, _ := url.Parse("http://www.example.com/")
	if h := j2.Header(plain); h != "sess=v3" {
		t.Fatalf("http header = %q (secure cookie over http)", h)
	}
	foreign, _ := url.Parse("https://notexample.com/")
	if h := j2.Header(foreign); h != "" {
		t.Fatalf("foreign header = %q", h)
	}
	if err := j2.Clear(); err != nil || !j2.Empty() {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("jar file kept after Clear")
	}
}

func testClient(t *testing.T, srv *httptest.Server) (*Client, *Jar) {
	t.Helper()
	j, _ := Open(filepath.Join(t.TempDir(), "j.json"))
	host := strings.TrimPrefix(srv.URL, "https://")
	h, _, _ := strings.Cut(host, ":")
	if err := j.Replace([]Cookie{{Name: "sid", Value: "abc", Domain: h, Path: "/", Secure: true}}, "UA/9", "", "window"); err != nil {
		t.Fatal(err)
	}
	hc := srv.Client()
	if tr, ok := hc.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: httptest certificate
	}
	return &Client{Jar: j, Hosts: []string{h}, HTTP: hc, UserAgent: "UA/9"}, j
}

func TestClientCookiesUserAgentAndIngest(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "sid=abc" && r.URL.Path == "/a" {
			http.Error(w, "cookie "+r.Header.Get("Cookie"), 400)
			return
		}
		if r.UserAgent() != "UA/9" {
			http.Error(w, "ua", 400)
			return
		}
		switch r.URL.Path {
		case "/a":
			http.SetCookie(w, &http.Cookie{Name: "XSRF-TOKEN", Value: "tok", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			http.SetCookie(w, &http.Cookie{Name: "evil", Value: "x", Domain: "other.org", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			_, _ = w.Write([]byte("ok"))
		case "/b":
			_, _ = w.Write([]byte(r.Header.Get("Cookie")))
		case "/away":
			http.Redirect(w, r, "https://example.org/", http.StatusFound)
		}
	}))
	defer srv.Close()
	c, j := testClient(t, srv)
	ctx := context.Background()
	if r, err := c.Do(ctx, "GET", srv.URL+"/a", nil, nil, false); err != nil || r.Status != 200 {
		t.Fatalf("a: %+v %v", r, err)
	}
	if j.Value("XSRF-TOKEN") != "tok" || j.Value("evil") != "" {
		t.Fatalf("ingest: xsrf=%q evil=%q", j.Value("XSRF-TOKEN"), j.Value("evil"))
	}
	r, err := c.Do(ctx, "GET", srv.URL+"/b", nil, nil, false)
	if err != nil || string(r.Body) != "sid=abc; XSRF-TOKEN=tok" {
		t.Fatalf("b: %q %v", r.Body, err)
	}
	r, _ = c.Do(ctx, "GET", srv.URL+"/b", nil, nil, true)
	if string(r.Body) != "" {
		t.Fatalf("anonymous request sent cookies: %q", r.Body)
	}
	if _, err := c.Do(ctx, "GET", srv.URL+"/away", nil, nil, false); !errors.Is(err, ErrHost) {
		t.Fatalf("redirect off the hosts: err = %v", err)
	}
	if _, err := c.Do(ctx, "GET", "https://example.org/", nil, nil, false); !errors.Is(err, ErrHost) {
		t.Fatalf("foreign host: err = %v", err)
	}
}

// challengePage is a saved (trimmed) Cloudflare managed-challenge answer.
const challengePage = `<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title><meta http-equiv="refresh" content="360"></head><body><div class="main-wrapper" role="main"><div class="main-content"><noscript>Enable JavaScript and cookies to continue</noscript></div></div><script>(function(){window._cf_chl_opt = {cvId: '3',cZone: 'www.nexusmods.com',cType: 'managed'};var a = document.createElement('script');a.src = '/cdn-cgi/challenge-platform/h/g/orchestrate/chl_page/v1?ray=abc';})();</script></body></html>`

func TestChallengeDetection(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hdr" {
			w.Header().Set("cf-mitigated", "challenge")
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(challengePage))
	}))
	defer srv.Close()
	c, _ := testClient(t, srv)
	for _, p := range []string{"/hdr", "/body"} {
		if _, err := c.Do(context.Background(), "GET", srv.URL+p, nil, nil, false); !errors.Is(err, ErrChallenge) {
			t.Fatalf("%s: err = %v", p, err)
		}
	}
	if Challenged(200, http.Header{}, []byte("<title>Just a moment with the author</title>")) {
		t.Fatal("a 200 page mentioning the words is not a challenge")
	}
}
