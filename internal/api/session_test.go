package api

import (
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"testing/fstest"
)

// A persistent session secret keeps a browser signed in across app restarts
// (a new server, a new bearer token), and the cookie outlives the browser session.
func TestPersistentSessionSurvivesRestart(t *testing.T) {
	secret := strings.Repeat("0123456789abcdef", 4) // dummy fixture, not a real secret
	start := func() *Server {
		s, err := New(t.Context(), Options{
			Assets: fstest.MapFS{"index.html": {Data: []byte(indexHTML)}},
			Open:   func(string) {}, Log: slog.New(slog.DiscardHandler),
			SessionSecret: secret,
		})
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = s.Serve() }()
		t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
		return s
	}
	first := start()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, first.LaunchURL("/"), nil)
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Request.Response.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.MaxAge <= 0 || cookie.Value != secret {
		t.Fatalf("cookie %+v", cookie)
	}

	second := start() // "restart": other port, other bearer token
	if second.Token() == first.Token() {
		t.Fatal("bearer token must still rotate per run")
	}
	// Cookies are not port-specific: the same browser calls the new instance.
	req2, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, second.BaseURL()+"/api/health", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie.Value}) //nolint:gosec // G124: client-side test cookie, attributes irrelevant
	r2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("restarted server rejected the session: %d", r2.StatusCode)
	}

	// Without a persistent secret each run has its own session.
	other, err := New(t.Context(), Options{Assets: fstest.MapFS{}, Open: func(string) {}, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.ln.Close() })
	if strings.EqualFold(other.session, secret) || len(other.session) != 64 {
		t.Fatalf("per-run session %q", other.session)
	}
}
