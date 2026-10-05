//go:build e2e

// Owner-data switch end to end (TASKS.md Phase 6 step 13): an owner-DB-like
// store synced by the MCP engine (MCP-era poll fingerprints, empty
// providers.<id>.author, no engine key, links / decisions / jobs / unread)
// restarts on the native default against fake sites, signed out.
//
//	go test -tags e2e -run TestOwnerDataSwitchE2E -v -timeout 10m ./cmd/issuewatcher
//
// Expected: the stored accounts are adopted as authors, only the projects
// whose fingerprints lack "v2:" are re-read (silently: zero new-item events,
// no full source reconcile, so no ProgressStarted), a project already on v2 is
// only checked, and every table keeps its row count.
package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/factorio"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

func providerFixture(t *testing.T, pkg, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "provider", pkg, "testdata", name)) //nolint:gosec // G304: fixed test fixtures
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ownerSites is one TLS server for the keyless native reads (Nexus v2
// GraphQL, CurseForge comments + CFWidget, the Factorio portal) with the
// owner's project ids (windrose/147, 1437738, auto-build-and-deconstruct).
type ownerSites struct {
	t   *testing.T
	srv *httptest.Server
	hc  *http.Client

	mu   sync.Mutex
	hits map[string]int // request kind → count
}

func (s *ownerSites) hit(kind string) {
	s.mu.Lock()
	s.hits[kind]++
	s.mu.Unlock()
}

// takeHits returns the counts since the last call.
func (s *ownerSites) takeHits() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hits
	s.hits = map[string]int{}
	return h
}

func newOwnerSites(t *testing.T) *ownerSites {
	s := &ownerSites{t: t, hits: map[string]int{}}
	cf := map[string]string{"0": providerFixture(t, "curseforge", "comments_0.json"), "1": providerFixture(t, "curseforge", "comments_1.json")}
	fList, fThread, fUser := providerFixture(t, "factorio", "list_1.html"), providerFixture(t, "factorio", "thread.html"), providerFixture(t, "factorio", "user.html")
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/v2/graphql" || p == "/graphql":
			var q struct {
				Query string `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&q)
			switch {
			case strings.Contains(q.Query, "game(domainName"):
				_, _ = w.Write([]byte(`{"data":{"game":{"id":8851}}}`))
			case strings.Contains(q.Query, "mods(filter"):
				_, _ = w.Write([]byte(`{"data":{"mods":{"totalCount":1,"nodes":[{"modId":147,"name":"ShareShip","description":"","uploader":{"name":"UberMorgott","memberId":6541781},"game":{"domainName":"windrose"}}]}}}`))
			default: // preferences etc.: signed out
				_, _ = w.Write([]byte(`{"errors":[{"message":"Unauthorized","extensions":{"code":"UNAUTHORIZED"}}],"data":{"preferences":null}}`))
			}
		case p == "/author/search/Morgott":
			_, _ = w.Write([]byte(`{"id":136845864,"username":"Morgott","projects":[{"id":1437738,"name":"Nick Name Changer"}]}`))
		case p == "/1437738":
			_, _ = w.Write([]byte(`{"summary":"","urls":{"curseforge":"https://www.curseforge.com/hytale/mods/nick-name-changer"}}`))
		case p == "/api/v1/users/profile":
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		case p == "/api/v1/mods/1437738/comments":
			s.hit("curseforge comments")
			body, ok := cf[r.URL.Query().Get("page")]
			if !ok {
				body = `{"data":[],"pagination":{"index":9,"totalCount":82,"pageSize":20}}`
			}
			_, _ = w.Write([]byte(body))
		case p == "/user/Morgott":
			_, _ = w.Write([]byte(fUser))
		case p == "/api/mods/auto-build-and-deconstruct/full":
			_, _ = w.Write([]byte(`{"name":"auto-build-and-deconstruct","title":"Lazy Builder","source_url":""}`))
		case p == "/":
			_, _ = w.Write([]byte(`<div class="header"><a href="/login">Log in</a></div><div class="container"></div>`))
		case p == "/mod/auto-build-and-deconstruct/discussion":
			s.hit("factorio list")
			_, _ = w.Write([]byte(fList))
		case strings.HasSuffix(p, "/discussion") || strings.Contains(p, "/discussion/page/"):
			_, _ = w.Write([]byte(`<table></table>`))
		case strings.Contains(p, "/discussion/") && r.Method == http.MethodGet:
			s.hit("factorio thread")
			_, _ = w.Write([]byte(fThread))
		case r.Method != http.MethodGet && r.Method != http.MethodHead:
			t.Errorf("write request %s %s: the switch must never post", r.Method, p)
			http.Error(w, "no writes", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	s.hc = s.srv.Client()
	if tr, ok := s.hc.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: httptest certificate
	}
	return s
}

// nexusWindow is the installed browser for Nexus: www.nexusmods.com widgets.
type nexusWindow struct {
	*fakeWindow
	t        *testing.T
	sites    *ownerSites
	comments map[int]string
	bugs     string
	bugPosts map[string]string
}

func newNexusWindow(t *testing.T, host string, sites *ownerSites) *nexusWindow {
	return &nexusWindow{fakeWindow: &fakeWindow{host: host}, t: t, sites: sites,
		comments: map[int]string{1: providerFixture(t, "nexus", "comments_1.html"), 2: providerFixture(t, "nexus", "comments_2.html")},
		bugs:     providerFixture(t, "nexus", "bugs_1.html"),
		bugPosts: map[string]string{"1062068": providerFixture(t, "nexus", "bug_0.html"), "958735": providerFixture(t, "nexus", "bug_1.html")}}
}

func (w *nexusWindow) Fetch(_ context.Context, req browser.Request) (browser.Response, error) {
	u, _ := url.Parse(req.URL)
	ok := func(body string) (browser.Response, error) {
		return browser.Response{Status: 200, URL: req.URL, Body: body}, nil
	}
	switch {
	case req.Method == http.MethodPost && !strings.Contains(req.URL, "ModBugReplyList"):
		w.t.Errorf("nexus write %s: the switch must never post", req.URL)
		return browser.Response{Status: 403, URL: req.URL}, nil
	case strings.HasSuffix(u.Path, "/mods/147"):
		return ok(`<div data-target="/Core/Libs/Common/Widgets/CommentContainer?game_id=8851&amp;object_id=147&amp;object_type=1&amp;thread_id=16796543"></div>`)
	case strings.Contains(req.URL, "CommentContainer"):
		w.sites.hit("nexus comments")
		var page int
		_, _ = fmt.Sscanf(req.URL[strings.LastIndex(req.URL, "page:")+5:], "%d", &page)
		body, found := w.comments[page]
		if !found {
			body = w.comments[1]
		}
		return ok(body)
	case strings.Contains(req.URL, "ModBugsTab"):
		return ok(w.bugs)
	case strings.Contains(req.URL, "ModBugReplyList"):
		form, _ := url.ParseQuery(req.Body)
		body, found := w.bugPosts[form.Get("issue_id")]
		if !found {
			body = providerFixture(w.t, "nexus", "bug_deleted.html")
		}
		return ok(body)
	}
	return browser.Response{Status: 404, URL: req.URL}, nil
}

// ownerApp is one start of the mod platforms on the data dir (the production
// modPlatforms + sync group; native engines over the fake sites).
type ownerApp struct {
	m      *modPlatforms
	cfgs   *config.Store
	db     *sql.DB
	st     *store.Store
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	events   []store.Event
	progress []syncer.Progress
}

func startOwnerApp(t *testing.T, dir string, sites *ownerSites) *ownerApp {
	t.Helper()
	cfgs, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	onDisk := cfgs.Get().Providers
	db, err := store.Open(t.Context(), filepath.Join(dir, "issuewatcher.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := &ownerApp{cfgs: cfgs, db: db, st: store.New(db), done: make(chan struct{})}
	log := slog.New(slog.NewTextHandler(testLog{t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	gh := github.NewProvider(newAuth(log, dir))
	stm := steam.New(steam.Options{Dir: filepath.Join(dir, "secrets"), Log: log})
	group := syncer.NewGroup()
	group.OnProgress(func(p syncer.Progress) { a.mu.Lock(); a.progress = append(a.progress, p); a.mu.Unlock() })
	onUpdate := func(evs []store.Event, _ int) { a.mu.Lock(); a.events = append(a.events, evs...); a.mu.Unlock() }
	// The file keeps the platforms off until the fakes are wired: switching
	// them on runs the same apply (start + account adoption) as a start.
	if onDisk.Nexus.Enabled || onDisk.CurseForge.Enabled || onDisk.Factorio.Enabled {
		t.Fatal("seed config must keep the platforms off on disk")
	}
	a.m = newModPlatforms(cfgs, a.st, log, group, gh, stm, onUpdate, dir, nil)
	host, _, _ := strings.Cut(strings.TrimPrefix(sites.srv.URL, "https://"), ":")
	a.m.native = func(id string, author func() string) (provider.Provider, *signin.Manager) {
		jar, _ := websession.Open(filepath.Join(dir, "secrets", id+".json"))
		spec := signin.Spec{Platform: id, LoginURL: "https://example.invalid/login", Interval: time.Second, Timeout: time.Second,
			Probe: func(context.Context, *websession.Jar) (string, error) {
				return "", fmt.Errorf("%w: test", provider.ErrNotSignedIn)
			}}
		switch id {
		case nexus.Platform:
			nw := newNexusWindow(t, host, sites)
			mgr := signin.New(spec, jar, nw)
			return nexus.New(nexus.Options{Native: &nexus.NativeOptions{Browser: nw, Session: mgr, HTTP: sites.hc,
				GraphQL: sites.srv.URL + "/v2/graphql", APIRouter: sites.srv.URL + "/graphql"}, Author: author, Log: log}), mgr
		case curseforge.Platform:
			fw := &fakeWindow{host: host}
			mgr := signin.New(spec, jar, fw)
			return curseforge.New(curseforge.Options{Native: &curseforge.NativeOptions{Browser: fw, Session: mgr, HTTP: sites.hc,
				Site: sites.srv.URL, Widget: sites.srv.URL}, Author: author, Log: log}), mgr
		}
		mgr := signin.New(spec, jar, &fakeWindow{host: host})
		return factorio.New(factorio.Options{Session: mgr, HTTP: sites.hc, Site: sites.srv.URL, Author: author, Log: log}), mgr
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go func() { group.Run(ctx); close(a.done) }()
	on, err := cfgs.Patch(cfgs.Get().Revision, []byte(`{"providers":{"nexus":{"enabled":true},"curseforge":{"enabled":true},"factorio":{"enabled":true}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	a.m.apply(on)
	return a
}

func (a *ownerApp) stop() {
	a.cancel()
	<-a.done
	a.m.Close()
	_ = a.db.Close()
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func waitUntil(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func queryInt(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

var ownerTables = []string{"sources", "projects", "items", "comments", "project_links", "project_link_decisions", "jobs"}

func rowCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{"unread": queryInt(t, db, `SELECT count(*) FROM items WHERE unread = 1`)}
	for _, tb := range ownerTables {
		out[tb] = queryInt(t, db, `SELECT count(*) FROM `+tb) //nolint:gosec // G202: fixed table names
	}
	return out
}

type modProject struct {
	id                 int64
	platform, synced   string
	poll               provider.PollState
	sourceID           int64
	sourceReconciledAt string
}

func modProjects(t *testing.T, db *sql.DB) map[string]modProject {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT p.id, p.external_id, s.platform, p.synced_at, p.poll_state, s.id, s.reconciled_at
		FROM projects p JOIN sources s ON s.id = p.source_id WHERE s.platform IN ('nexus', 'curseforge', 'factorio')`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]modProject{}
	for rows.Next() {
		var p modProject
		var poll, ext string
		if err := rows.Scan(&p.id, &ext, &p.platform, &p.synced, &poll, &p.sourceID, &p.sourceReconciledAt); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(poll), &p.poll); err != nil {
			t.Fatal(err)
		}
		out[ext] = p
	}
	return out
}

func hasV2(st provider.PollState) bool {
	for k := range st.ETags {
		if strings.HasPrefix(k, "v2:") {
			return true
		}
	}
	return false
}

func TestOwnerDataSwitchE2E(t *testing.T) {
	sites := newOwnerSites(t)
	dir := t.TempDir()
	writeConfig := func(version int, providers map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"schemaVersion": version, "updates": map[string]any{"autoCheck": false}, "providers": providers})
		if err := os.WriteFile(filepath.Join(dir, config.File), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 1. The owner's data as the MCP engine left it: one source per platform,
	// its projects, items and comments (seeded here by one sync over the fakes).
	writeConfig(config.SchemaVersion, map[string]any{
		"nexus":      map[string]any{"enabled": false, "author": "UberMorgott"},
		"curseforge": map[string]any{"enabled": false, "author": "Morgott"},
		"factorio":   map[string]any{"enabled": false, "author": "Morgott"},
	})
	seed := startOwnerApp(t, dir, sites)
	waitUntil(t, "the seed sync of three sources", 60*time.Second, func() bool {
		return queryInt(t, seed.db, `SELECT count(*) FROM sources WHERE platform IN ('nexus', 'curseforge', 'factorio') AND reconciled_at <> ''`) == 3 &&
			queryInt(t, seed.db, `SELECT count(DISTINCT source_id) FROM items`) == 3
	})
	seed.stop()

	// 2. MCP-era state: fingerprints without "v2:" on Nexus + CurseForge, a
	// current v2 fingerprint on Factorio (checked by a native build already),
	// old synced/checked times, recent reconciles; a link, a decision, a job,
	// unread items; the owner's config (no engine key, empty authors).
	db, err := store.Open(t.Context(), filepath.Join(dir, "issuewatcher.db"))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(db)
	const nx, cfp, fa, fb = "windrose/147", "1437738", "auto-build-and-deconstruct", "Turrets-need-energy-source"
	before := modProjects(t, db)
	if len(before) != 4 || before[nx].platform != nexus.Platform || before[cfp].platform != curseforge.Platform || before[fa].platform != factorio.Platform {
		t.Fatalf("seed projects: %+v", before)
	}
	fprov := factorio.New(factorio.Options{HTTP: sites.hc, Site: sites.srv.URL, Author: func() string { return "Morgott" }})
	for _, ext := range []string{fa, fb} {
		p := before[ext]
		if _, err := fprov.DetectChanges(t.Context(), provider.Project{ExternalID: ext}, &p.poll); err != nil || !hasV2(p.poll) {
			t.Fatalf("factorio fingerprint %s: %v %v", ext, p.poll.ETags, err)
		}
		if err := st.SavePollState(t.Context(), p.id, p.poll, time.Now().Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	old, full := "2026-09-29T20:11:14Z", fmt.Sprint(time.Now().Add(-8*time.Hour).Unix()) // as the owner DB: MCP keys, full read hours ago, no fullAt
	for _, ext := range []string{nx, cfp} {
		p := before[ext]
		p.poll.ETags = map[string]string{p.platform + ":page1": "4d16e9d0150da29c8ba0a815", p.platform + ":page1@full": full}
		p.poll.FullAt = time.Time{}
		if err := st.SavePollState(t.Context(), p.id, p.poll, time.Now().Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	recent := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	for _, q := range []string{
		`UPDATE projects SET synced_at = '` + old + `' WHERE source_id IN (SELECT id FROM sources WHERE platform IN ('nexus', 'curseforge', 'factorio'))`,
		`UPDATE sources SET reconciled_at = '` + recent + `' WHERE platform IN ('nexus', 'curseforge', 'factorio')`,
		`INSERT INTO sources (platform, account) VALUES ('github', 'UberMorgott')`,
		`INSERT INTO projects (source_id, external_id, name) SELECT id, 'UberMorgott/ShareShip', 'ShareShip' FROM sources WHERE platform = 'github'`,
		`INSERT INTO project_links (mod_project_id, code_project_id) SELECT m.id, c.id FROM projects m, projects c
			WHERE m.id = ` + fmt.Sprint(before[nx].id) + ` AND c.external_id = 'UberMorgott/ShareShip'`,
		`INSERT INTO project_link_decisions (mod_project_id) VALUES (` + fmt.Sprint(before[cfp].id) + `)`,
		`INSERT INTO jobs (item_id, project_id, flow, state) SELECT id, project_id, 'reply', 'done' FROM items
			WHERE project_id = ` + fmt.Sprint(before[nx].id) + ` ORDER BY id LIMIT 1`,
		`UPDATE items SET unread = CASE WHEN id % 3 = 0 THEN 1 ELSE 0 END`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	want := rowCounts(t, db)
	before = modProjects(t, db)
	_ = db.Close()
	writeConfig(6, map[string]any{ // v6: the MCP engine's keys, dropped by the v7 migration
		"nexus":      map[string]any{"enabled": false, "author": "", "engine": "mcp", "mcp": map[string]any{"command": "server", "args": []string{"index.js"}}},
		"curseforge": map[string]any{"enabled": false, "author": "", "mcp": map[string]any{"command": "server", "args": []string{"index.js"}}},
		"factorio":   map[string]any{"enabled": false, "author": "Morgott"},
	})

	// 3. First native start, signed out everywhere.
	sites.takeHits()
	a := startOwnerApp(t, dir, sites)
	defer a.stop()
	got := a.cfgs.Get().Providers
	if body, err := os.ReadFile(filepath.Join(dir, config.File)); err != nil || strings.Contains(string(body), `"mcp"`) || strings.Contains(string(body), `"engine"`) {
		t.Fatalf("config.json keeps the MCP engine keys: %s %v", body, err)
	}
	if got.Nexus.Author != "UberMorgott" || got.CurseForge.Author != "Morgott" || got.Factorio.Author != "Morgott" {
		t.Fatalf("adopted authors: nexus %q, curseforge %q, factorio %q", got.Nexus.Author, got.CurseForge.Author, got.Factorio.Author)
	}
	checked := func(id int64) bool {
		return queryInt(t, a.db, `SELECT count(*) FROM projects WHERE id = ? AND checked_at > ?`, id, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)) == 1
	}
	deadline := time.Now().Add(150 * time.Second)
	for {
		now := modProjects(t, a.db)
		if hasV2(now[nx].poll) && hasV2(now[cfp].poll) && checked(before[fa].id) && checked(before[fb].id) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the re-fingerprint: %+v", now)
		}
		time.Sleep(250 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // the check's own writes and events land
	hits := sites.takeHits()
	// A v2-less fingerprint forces a re-read beyond the page-1 check; the
	// Factorio check (already v2, unchanged) reads the list page only.
	if hits["curseforge comments"] < 2 || hits["nexus comments"] < 2 || hits["factorio list"] < 1 || hits["factorio thread"] != 0 {
		t.Errorf("site reads on the switch: %v", hits)
	}
	after := modProjects(t, a.db)
	for ext, p := range after {
		if p.sourceID != before[ext].sourceID || p.id != before[ext].id || p.sourceReconciledAt != before[ext].sourceReconciledAt {
			t.Errorf("%s: source/project/reconcile moved: %+v → %+v", ext, before[ext], p)
		}
	}
	for _, ext := range []string{fa, fb} {
		if after[ext].synced != old || after[ext].poll.ETags["v2:factorio:page1"] != before[ext].poll.ETags["v2:factorio:page1"] {
			t.Errorf("factorio %s (already v2) re-read: synced %s, etags %v", ext, after[ext].synced, after[ext].poll.ETags)
		}
	}
	if n := rowCounts(t, a.db); fmt.Sprint(n) != fmt.Sprint(want) {
		t.Errorf("row counts: before %v, after %v", want, n)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.events {
		if e.Kind == store.EventNewItem || e.Kind == store.EventNewIssue || e.Kind == store.EventNewComment {
			t.Errorf("event on the switch: %+v", e)
		}
	}
	for _, p := range a.progress {
		if p.State == syncer.ProgressStarted || p.State == syncer.ProgressError {
			t.Errorf("full reconcile / error on the switch: %+v", p)
		}
	}
	t.Logf("rows %v; reads %v; events %d; progress %d; v2: nexus %v, curseforge %v", want, hits, len(a.events), len(a.progress), hasV2(after[nx].poll), hasV2(after[cfp].poll))
}
