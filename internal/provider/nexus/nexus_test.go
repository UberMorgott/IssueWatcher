package nexus_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge/mcptest"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// site is the fake Nexus state behind the fake MCP server.
type site struct {
	mu        sync.Mutex
	pages     map[int][]map[string]any // Posts page → root threads
	bugs      []map[string]any
	bugPosts  map[string]map[string]any // issue id → get_mod_bug result
	failPage  int                       // get_mod_comments page answering an error
	crashOnce bool
	bugsOff   bool
}

func ts(min int) string {
	return time.Date(2026, 9, 1, 10, min, 0, 0, time.UTC).Format(time.RFC3339)
}

func thread(id, author, body string, min int, replies ...map[string]any) map[string]any {
	if replies == nil {
		replies = []map[string]any{}
	}
	return map[string]any{"id": id, "parentId": nil, "author": author, "authorId": 1, "isModAuthor": false,
		"createdAt": ts(min), "updatedAt": nil, "body": body, "sticky": false, "locked": false, "replies": replies}
}

func reply(id, parent, author, body string, min int) map[string]any {
	return map[string]any{"id": id, "parentId": parent, "author": author, "authorId": 2, "isModAuthor": false,
		"createdAt": ts(min), "updatedAt": nil, "body": body}
}

func newSite() *site {
	return &site{
		pages: map[int][]map[string]any{
			1: {thread("103", "carol", "Crash on load\nfull log below", 30), thread("102", "bob", "Works great", 20,
				reply("201", "102", "UberMorgott", "thanks!", 21))},
			// Shifted page: 102 moved down while listing (a new thread on page 1).
			2: {thread("102", "bob", "Works great", 20, reply("201", "102", "UberMorgott", "thanks!", 21)), thread("101", "alice", "First!", 10)},
		},
		bugs: []map[string]any{{"id": "900", "title": "Ship UI missing", "status": "New issue", "statusKey": "new", "open": true,
			"replies": 1, "version": "1.3.0", "priority": "Not set", "lastPostAt": ts(40)}},
		bugPosts: map[string]map[string]any{"900": {"issueId": "900", "canReply": true,
			"report": map[string]any{"id": "900", "parentId": nil, "author": "dave", "authorId": 3, "createdAt": nil, "createdAtLocal": "2026-09-01T13:35", "body": "UI gone after update"},
			"replies": []map[string]any{{"id": "901", "parentId": "900", "author": "UberMorgott", "authorId": 1, "createdAt": nil, "createdAtLocal": "2026-09-01T13:40", "body": "looking"}}}},
	}
}

func (s *site) install(f *mcptest.Server) {
	f.Handle("search_mods", func(args map[string]any) (any, error) {
		if args["author"] != "Morgott" {
			return map[string]any{"total": 0, "offset": 0, "count": 50, "mods": []any{}}, nil
		}
		return map[string]any{"total": 1, "offset": 0, "count": 50, "mods": []any{map[string]any{
			"game": "windrose", "modId": 147, "name": "ShareShip", "url": "https://www.nexusmods.com/windrose/mods/147",
			"author": "Morgott", "uploader": map[string]any{"name": "UberMorgott", "memberId": 6541781}}}}, nil
	})
	f.Handle("get_mod_comments", func(args map[string]any) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.crashOnce {
			s.crashOnce = false
			return nil, mcptest.ErrCrash
		}
		page := int(args["page"].(float64)) //nolint:forcetypeassert // test fixture
		if page == s.failPage {
			return nil, &mcptest.CodeError{Code: "cloudflare", Message: "challenge"}
		}
		total := 0
		for _, p := range s.pages {
			total += len(p)
		}
		return map[string]any{"game": "windrose", "modId": 147, "threadId": 5, "page": page, "pages": len(s.pages),
			"perPage": 10, "total": total, "url": "u", "comments": s.pages[page]}, nil
	})
	f.Handle("get_mod_bugs", func(map[string]any) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.bugsOff {
			return nil, &mcptest.CodeError{Code: mcpbridge.CodeDisabled, Message: "off"}
		}
		return map[string]any{"game": "windrose", "modId": 147, "filter": "all", "page": 1, "pages": 1, "perPage": 10,
			"canReport": true, "url": "https://www.nexusmods.com/windrose/mods/147?tab=bugs", "bugs": s.bugs}, nil
	})
	f.Handle("get_mod_bug", func(args map[string]any) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		r, ok := s.bugPosts[fmt.Sprint(args["issue_id"])]
		if !ok {
			return nil, &mcptest.CodeError{Code: mcpbridge.CodeNotFound, Message: "gone"}
		}
		return r, nil
	})
}

type env struct {
	site  *site
	fake  *mcptest.Server
	prov  *nexus.Provider
	sy    *syncer.Syncer
	db    *sql.DB
	store *store.Store
	evs   chan []store.Event
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{site: newSite(), fake: mcptest.New(), evs: make(chan []store.Event, 8)}
	e.site.install(e.fake)
	b := mcpbridge.New(mcpbridge.Options{Name: "nexus", Dial: e.fake.Dial, CallTimeout: 5 * time.Second})
	t.Cleanup(b.Close)
	e.prov = nexus.New(nexus.Options{Bridge: b, Author: func() string { return "Morgott" }})
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e.db, e.store = db, store.New(db)
	e.sy = syncer.New(syncer.Options{Store: e.store, Provider: e.prov,
		OnUpdate: func(evs []store.Event, _ int) { e.evs <- evs }})
	return e
}

func (e *env) items(t *testing.T) map[string]string {
	t.Helper()
	rows, err := e.db.QueryContext(t.Context(), `SELECT i.external_id, i.kind, i.title, i.status, i.raw_status,
		(SELECT count(*) FROM comments c WHERE c.item_id = i.id) FROM items i`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var id, kind, title, status, raw string
		var n int
		if err := rows.Scan(&id, &kind, &title, &status, &raw, &n); err != nil {
			t.Fatal(err)
		}
		out[id] = fmt.Sprintf("%s|%s|%s|%s|%d", kind, title, status, raw, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSyncStoresCommentsAndBugs(t *testing.T) {
	e := setup(t)
	if err := e.sy.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-e.evs
	got := e.items(t)
	want := map[string]string{
		"comment:windrose/147/103": "comment|Crash on load|open||0",
		"comment:windrose/147/102": "comment|Works great|open||1",
		"comment:windrose/147/101": "comment|First!|open||0",
		"bug:900":                  "bug|Ship UI missing|open|new|1",
	}
	if len(got) != len(want) {
		t.Fatalf("items %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	var account, project string
	if err := e.db.QueryRowContext(t.Context(), `SELECT s.account, p.external_id FROM projects p JOIN sources s ON s.id = p.source_id`).
		Scan(&account, &project); err != nil {
		t.Fatal(err)
	}
	if account != "UberMorgott" || project != "windrose/147" {
		t.Fatalf("source %q project %q", account, project)
	}

	// New replies: someone else's notifies, the account's own does not; the
	// unchanged bug report is not re-read.
	e.site.mu.Lock()
	p1 := e.site.pages[1]
	p1[0]["replies"] = []map[string]any{reply("202", "103", "erin", "same here", 50), reply("203", "103", "UberMorgott", "fixed in 1.3.1", 51)}
	e.site.mu.Unlock()
	if err := e.sy.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	evs := <-e.evs
	if len(evs) != 1 || evs[0].Kind != store.EventNewComment || evs[0].Actor != "erin" || evs[0].ItemKind != store.KindComment {
		t.Fatalf("events %+v", evs)
	}
	if n := e.fake.Count("get_mod_bug"); n != 1 {
		t.Fatalf("get_mod_bug called %d times, want 1 (cached)", n)
	}
}

func TestErrorMidListingKeepsCursorAndProjects(t *testing.T) {
	e := setup(t)
	if err := e.sy.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-e.evs
	var cursor0 string
	_ = e.db.QueryRowContext(t.Context(), `SELECT sync_cursor FROM projects`).Scan(&cursor0)

	e.site.mu.Lock()
	e.site.failPage = 2
	e.site.pages[1] = append([]map[string]any{thread("104", "frank", "new one", 59)}, e.site.pages[1]...)
	e.site.mu.Unlock()
	if err := e.sy.SyncOnce(t.Context()); err == nil {
		t.Fatal("want the page-2 error")
	}
	<-e.evs
	var cursor string
	var active int
	if err := e.db.QueryRowContext(t.Context(), `SELECT sync_cursor, active FROM projects`).Scan(&cursor, &active); err != nil {
		t.Fatal(err)
	}
	if cursor != cursor0 || active != 1 {
		t.Fatalf("cursor %q → %q, active %d", cursor0, cursor, active)
	}
	if _, ok := e.items(t)["comment:windrose/147/104"]; ok {
		t.Fatal("a partial listing must not be stored")
	}
}

func TestCrashRestartsAndBugsDisabled(t *testing.T) {
	e := setup(t)
	e.site.crashOnce = true
	e.site.bugsOff = true
	if err := e.sy.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-e.evs
	if e.fake.Starts() != 2 {
		t.Fatalf("starts = %d, want 2", e.fake.Starts())
	}
	got := e.items(t)
	if len(got) != 3 {
		t.Fatalf("items %v", got)
	}
}

func TestMissingServerOrAuthorIsSignedOut(t *testing.T) {
	b := mcpbridge.New(mcpbridge.Options{Name: "nexus", Command: func() (string, []string) {
		return "node", []string{filepath.Join(t.TempDir(), "missing", "index.js")}
	}})
	p := nexus.New(nexus.Options{Bridge: b, Author: func() string { return "Morgott" }})
	_, err := p.Account(t.Context())
	if !errors.Is(err, provider.ErrNotSignedIn) || !errors.Is(err, mcpbridge.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	p = nexus.New(nexus.Options{Bridge: b})
	if _, err := p.Account(t.Context()); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("no author: %v", err)
	}
}

func TestDetectChanges(t *testing.T) {
	e := setup(t)
	pr := provider.Project{ExternalID: "windrose/147"}
	var st provider.PollState
	ch, err := e.prov.DetectChanges(t.Context(), pr, &st)
	if err != nil || ch.Overflow || ch.Requests != 2 {
		t.Fatalf("first check %+v %v", ch, err)
	}
	if ch, _ = e.prov.DetectChanges(t.Context(), pr, &st); ch.Overflow {
		t.Fatal("unchanged page 1 reported a change")
	}
	e.site.mu.Lock()
	e.site.bugs[0]["replies"] = 2
	e.site.mu.Unlock()
	if ch, _ = e.prov.DetectChanges(t.Context(), pr, &st); !ch.Overflow {
		t.Fatal("a new bug reply was not detected")
	}
}
