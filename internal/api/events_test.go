package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// openStream connects an SSE client and returns a line reader.
func openStream(t *testing.T, s *Server) *bufio.Reader {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL()+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(resp.Body)
	waitFor(t, func() bool { return s.Clients() == 1 })
	return br
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readEvent returns the next "event:"/"data:" pair, skipping comments.
func readEvent(t *testing.T, br *bufio.Reader) (string, string) {
	t.Helper()
	var name, data string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && name != "":
			return name, data
		}
	}
}

func TestEventsRequireAuth(t *testing.T) {
	s, _ := newTestServer(t, 0)
	if r := get(t, http.DefaultClient, s.BaseURL()+"/api/events", ""); r.status != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", r.status)
	}
}

func TestPublishReachesClient(t *testing.T) {
	s, _ := newTestServer(t, 0)
	br := openStream(t, s)
	if n := s.Publish(EventItemNew, map[string]any{"id": 7, "title": "Crash"}); n != 1 {
		t.Fatalf("published to %d clients, want 1", n)
	}
	name, data := readEvent(t, br)
	if name != EventItemNew || data != `{"id":7,"title":"Crash"}` {
		t.Fatalf("got %s %s", name, data)
	}
}

// With a tab open, the tray steers it instead of opening a new browser tab.
func TestOpenNavigatesOpenTab(t *testing.T) {
	s, opened := newTestServer(t, 0)
	br := openStream(t, s)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, s.BaseURL()+"/api/open",
		strings.NewReader(`{"path":"/item/5"}`))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	if r := do(t, http.DefaultClient, req); r.status != http.StatusNoContent {
		t.Fatalf("open: status %d", r.status)
	}
	if name, data := readEvent(t, br); name != EventNavigate || data != `{"path":"/item/5"}` {
		t.Fatalf("got %s %s, want navigate /item/5", name, data)
	}
	s.OpenBrowser("") // tray click: focus only
	if name, data := readEvent(t, br); name != EventNavigate || data != `{"path":""}` {
		t.Fatalf("got %s %s, want navigate with empty path", name, data)
	}
	s.OpenBrowser("https://evil.example/") // off-site path is neutralised
	if _, data := readEvent(t, br); data != `{"path":"/"}` {
		t.Fatalf("got %s, want path /", data)
	}
	select {
	case u := <-opened:
		t.Fatalf("browser opened %q although a tab is connected", u)
	default:
	}
}

// The first sync is silent for notifications but must still tell open tabs that
// data changed, report per-project progress, and end with sync.status done.
func TestFirstSyncPublishesProgressAndDataChanged(t *testing.T) {
	e := newEnv(t)
	if _, err := e.auth.ConvertManifest(t.Context(), githubtest.ManifestCode, e.s.Port()); err != nil {
		t.Fatal(err)
	}
	v, c := github.PKCE()
	if _, err := e.auth.Exchange(t.Context(), e.gh.IssueCode(c), v, "r"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	e.gh.Mu.Lock()
	e.gh.Repos = []string{"octo/app", "octo/lib"}
	e.gh.Issues = []*githubtest.Issue{{ID: "I_1", Repo: "octo/app", Number: 1, Title: "crash", Author: "alice", Open: true, CreatedAt: t0, UpdatedAt: t0}}
	e.gh.Mu.Unlock()
	br := openStream(t, e.s)
	if err := e.sync.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		name, data := readEvent(t, br)
		var p syncer.Progress
		if name == EventSyncStatus {
			if err := json.Unmarshal([]byte(data), &p); err != nil {
				t.Fatal(err)
			}
			name += ":" + p.State
		}
		got = append(got, name)
		if p.State == syncer.ProgressDone {
			if p.Changed < 2 || p.Error != "" {
				t.Fatalf("done %+v", p)
			}
			break
		}
	}
	want := "sync.status:started data.changed sync.status:progress data.changed sync.status:progress sync.status:done"
	if strings.Join(got, " ") != want {
		t.Fatalf("events\n got %s\nwant %s", strings.Join(got, " "), want)
	}

	// A second, unchanged sync reports progress but no data change.
	if err := e.sync.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	for {
		name, data := readEvent(t, br)
		if name == EventDataChanged {
			t.Fatalf("data.changed on an unchanged sync: %s", data)
		}
		if strings.Contains(data, `"state":"done"`) {
			break
		}
	}

	// Local actions publish data.changed too.
	var page store.IssuePage
	e.call(t, http.MethodGet, "/api/items", "", &page)
	id := strconv.FormatInt(page.Items[0].ID, 10)
	if code := e.call(t, http.MethodPost, "/api/items/"+id+"/read", "", nil); code != http.StatusNoContent {
		t.Fatalf("read: %d", code)
	}
	if name, data := readEvent(t, br); name != EventDataChanged || data != `{"reason":"read","itemId":`+id+`}` {
		t.Fatalf("got %s %s", name, data)
	}
}

func TestShutdownEndsStreams(t *testing.T) {
	s, _ := newTestServer(t, 0)
	br := openStream(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown with an open stream: %v", err)
	}
	for {
		if _, err := br.ReadString('\n'); err != nil {
			return // stream closed
		}
	}
}
