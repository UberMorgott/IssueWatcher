package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// logRec records log messages.
type logRec struct {
	mu   sync.Mutex
	msgs []string
}

func (r *logRec) Enabled(context.Context, slog.Level) bool { return true }
func (r *logRec) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *logRec) WithGroup(string) slog.Handler            { return r }
func (r *logRec) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, rec.Message)
	r.mu.Unlock()
	return nil
}

func (r *logRec) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.msgs)
}

// openDecisions are the log lines that end an OpenBrowser call.
var openDecisions = []string{
	"api: navigate sent to the active tab",
	"api: no dashboard tab connected; opening a new tab",
	"api: navigate reached no tab; opening a new tab",
	"api: focused tab not connected; opening a new tab",
	"api: dashboard window not found by title; opening a new tab",
	"api: new tab still connecting; route queued",
}

func newOpenServer(t *testing.T, wait time.Duration) (*Server, chan string, *logRec) {
	t.Helper()
	rec := &logRec{}
	opened := make(chan string, 8)
	s, err := New(t.Context(), Options{
		Assets:  fstest.MapFS{"index.html": {Data: []byte("<html></html>")}},
		Version: "test",
		Open:    func(u string) { opened <- u },
		Log:     slog.New(rec),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.opener.wait = wait
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	return s, opened, rec
}

// launchNext is the route a launch URL redirects to.
func launchNext(t *testing.T, s *Server, u string) string {
	t.Helper()
	_, tok, ok := strings.Cut(u, "/auth?t=")
	if !ok {
		t.Fatalf("not a launch URL: %q", u)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launches[tok].next
}

func mustOpen(t *testing.T, s *Server, opened chan string, next string) {
	t.Helper()
	select {
	case u := <-opened:
		if got := launchNext(t, s, u); got != next {
			t.Fatalf("new tab at %q, want %q", got, next)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no new tab opened (want %q)", next)
	}
}

func noOpen(t *testing.T, opened chan string, within time.Duration) {
	t.Helper()
	select {
	case u := <-opened:
		t.Fatalf("unexpected new tab %q", u)
	case <-time.After(within):
	}
}

// A click while the tab just opened is still loading must not be dropped or
// stack another tab: the route waits and reaches the new tab once it connects.
func TestOpenQueuesUntilNewTabConnects(t *testing.T) {
	s, opened, rec := newOpenServer(t, time.Minute)
	s.OpenBrowser("") // tray click, nothing open
	mustOpen(t, s, opened, "/")

	s.OpenBrowser("/item/3") // card click while the tab loads
	s.OpenBrowser("")        // focus-only click keeps the queued route
	noOpen(t, opened, 50*time.Millisecond)

	br := openStream(t, s) // the new tab connects
	if name, data := readEvent(t, br); name != EventNavigate || data != `{"path":"/item/3"}` {
		t.Fatalf("got %s %s, want the queued navigate /item/3", name, data)
	}
	if !slices.Contains(rec.all(), "api: new tab connected; queued route delivered") {
		t.Fatalf("delivery not logged: %q", rec.all())
	}

	s.OpenBrowser("/issues") // launch settled: the tab is steered again
	if name, data := readEvent(t, br); name != EventNavigate || data != `{"path":"/issues"}` {
		t.Fatalf("got %s %s", name, data)
	}
	noOpen(t, opened, 50*time.Millisecond)
}

// A queued route is not lost when the new tab never connects: it opens in a
// new tab after the wait.
func TestOpenQueuedRouteOpensWhenTabNeverConnects(t *testing.T) {
	s, opened, rec := newOpenServer(t, 50*time.Millisecond)
	s.OpenBrowser("/issues")
	mustOpen(t, s, opened, "/issues")
	s.OpenBrowser("/item/3")
	mustOpen(t, s, opened, "/item/3")
	noOpen(t, opened, 200*time.Millisecond) // nothing queued for the second launch
	msgs := rec.all()
	for _, want := range []string{
		"api: new tab did not connect in time; opening the queued route in a new tab",
		"api: new tab did not connect in time",
	} {
		if !slices.Contains(msgs, want) {
			t.Fatalf("missing %q in %q", want, msgs)
		}
	}
}

// No silent drop: every OpenBrowser call logs exactly one decision and ends in
// a visible result (navigate, new tab, or a queued route).
func TestOpenBrowserAlwaysDecides(t *testing.T) {
	s, opened, rec := newOpenServer(t, time.Minute)
	focusOK := true
	s.opts.Focus = func() (string, bool) { return "IssueWatcher · Обзор - Chrome", focusOK }

	decide := func(next, want string) {
		t.Helper()
		before := len(rec.all())
		s.OpenBrowser(next)
		var got []string
		for _, m := range rec.all()[before:] {
			if slices.Contains(openDecisions, m) {
				got = append(got, m)
			}
		}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("OpenBrowser(%q) decisions %q, want [%q]", next, got, want)
		}
	}

	decide("/", "api: no dashboard tab connected; opening a new tab")
	mustOpen(t, s, opened, "/")
	decide("/item/3", "api: new tab still connecting; route queued")
	br := openStream(t, s)
	if name, data := readEvent(t, br); name != EventNavigate || data != `{"path":"/item/3"}` {
		t.Fatalf("got %s %s", name, data)
	}
	decide("/issues", "api: navigate sent to the active tab")
	if name, data := readEvent(t, br); name != EventNavigate || data != `{"path":"/issues"}` {
		t.Fatalf("got %s %s", name, data)
	}
	focusOK = false
	decide("/item/4", "api: dashboard window not found by title; opening a new tab")
	if name, _ := readEvent(t, br); name != EventSuperseded {
		t.Fatalf("got %s, want superseded", name)
	}
	mustOpen(t, s, opened, "/item/4")
	decide("", "api: new tab still connecting; route queued")
	noOpen(t, opened, 50*time.Millisecond)
}

// A path navigate reaches only the active (most recently connected) tab, never
// an older one.
func TestNavigateTargetsActiveTabOnly(t *testing.T) {
	s, opened := newTestServer(t, 0)
	s.opts.Focus = func() (string, bool) { return "IssueWatcher · Обзор - Chrome", true }
	older := openStreamN(t, s, 1)
	active := openStreamN(t, s, 2)

	s.OpenBrowser("/item/9")
	if name, data := readEvent(t, active); name != EventNavigate || data != `{"path":"/item/9"}` {
		t.Fatalf("active tab got %s %s", name, data)
	}
	if n := s.Publish(EventItemNew, map[string]int{"id": 1}); n != 2 {
		t.Fatalf("marker reached %d tabs, want 2", n)
	}
	if name, _ := readEvent(t, older); name != EventItemNew {
		t.Fatalf("older tab got %s before the marker, want no navigate", name)
	}
	noOpen(t, opened, 50*time.Millisecond)
}

// After a restart the open tab reconnects by itself: the start must not open
// a duplicate tab while it does, and opens one only when nothing comes back.
func TestOpenOnStartWaitsForReconnect(t *testing.T) {
	s, opened, rec := newOpenServer(t, time.Minute)
	go func() {
		time.Sleep(100 * time.Millisecond)
		openStream(t, s) // the old tab's EventSource retry
	}()
	s.OpenOnStart("/", time.Second)
	noOpen(t, opened, 50*time.Millisecond)
	if !slices.Contains(rec.all(), "api: dashboard tab reconnected on start; not opening a new tab") {
		t.Fatalf("decision not logged: %q", rec.all())
	}

	s2, opened2, _ := newOpenServer(t, time.Minute)
	start := time.Now()
	s2.OpenOnStart("/", 200*time.Millisecond)
	mustOpen(t, s2, opened2, "/")
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("opened after %s, before the grace ended", d)
	}
}
