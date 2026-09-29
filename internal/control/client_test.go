package control

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/instance"
)

const token = "secret-token"

// serve starts an API fake and publishes it in a fresh data dir.
func serve(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	port := portOf(t, srv)
	if err := instance.WriteRuntime(dir, instance.Runtime{PID: 1, Port: port, URL: srv.URL, Token: token}); err != nil {
		t.Fatal(err)
	}
	return New(dir), srv
}

func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	port, err := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func authed(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return false
	}
	return true
}

func TestItemsPassesFiltersAndCursor(t *testing.T) {
	var got string
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		got = r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[],"nextCursor":"n2","more":true}`))
	})
	out, err := c.Items(t.Context(), ItemQuery{Project: 7, State: "open", Label: "bug", Text: "crash", Unread: true, Limit: 5, Cursor: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "GET /api/items?cursor=c1&label=bug&limit=5&project=7&q=crash&state=open&unread=1"; got != want {
		t.Fatalf("request %q, want %q", got, want)
	}
	if !strings.Contains(string(out), `"nextCursor":"n2"`) {
		t.Fatalf("body %s", out)
	}
}

func TestStaleTokenIsAPIError(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	_, err := c.Projects(t.Context())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || ae.Message != "unauthorized" {
		t.Fatalf("err %v", err)
	}
}

func TestForeignHostRejected(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	port := portOf(t, srv)
	for _, u := range []string{
		"http://localhost:" + strconv.Itoa(port),
		"http://example.com:" + strconv.Itoa(port),
		"https://127.0.0.1:" + strconv.Itoa(port),
		"http://127.0.0.1:" + strconv.Itoa(port+1),
		"http://127.0.0.1:" + strconv.Itoa(port) + "/x",
		"http://user@127.0.0.1:" + strconv.Itoa(port),
	} {
		dir := t.TempDir()
		if err := instance.WriteRuntime(dir, instance.Runtime{Port: port, URL: u, Token: token}); err != nil {
			t.Fatal(err)
		}
		if _, err := New(dir).Projects(t.Context()); err == nil || !strings.Contains(err.Error(), "non-loopback") {
			t.Fatalf("%s: err %v", u, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests reached the server", hits.Load())
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	defer other.Close()
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	})
	_, err := c.Projects(t.Context())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusFound || !strings.Contains(ae.Message, "redirect") {
		t.Fatalf("err %v", err)
	}
	if leaked.Load() != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestNotRunning(t *testing.T) {
	if _, err := New(t.TempDir()).Status(t.Context()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("no runtime.json: %v", err)
	}
	// A crashed app leaves runtime.json behind: nothing listens on its port.
	gone := httptest.NewServer(http.NotFoundHandler())
	port := portOf(t, gone)
	gone.Close()
	dir := t.TempDir()
	if err := instance.WriteRuntime(dir, instance.Runtime{Port: port, URL: "http://127.0.0.1:" + strconv.Itoa(port), Token: token}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir).Status(t.Context()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stale runtime.json: %v", err)
	}
}

func TestItemJoinsComments(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/items/3":
			_, _ = w.Write([]byte(`{"id":3,"title":"t"}`))
		case "/api/items/3/comments":
			if r.URL.Query().Get("limit") != "10" {
				t.Errorf("comments query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"items":[{"id":1}],"nextCursor":"","more":false}`))
		default:
			http.NotFound(w, r)
		}
	})
	out, err := c.Item(t.Context(), 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"item":{"id":3`) || !strings.Contains(s, `"comments":{"items":[{"id":1}]`) {
		t.Fatalf("item %s", s)
	}
}
