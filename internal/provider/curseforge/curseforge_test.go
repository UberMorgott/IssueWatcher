package curseforge_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge/mcptest"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

func ts(min int) string {
	return time.Date(2026, 9, 1, 10, min, 0, 0, time.UTC).Format(time.RFC3339)
}

func c(id, author, body string, min int) map[string]any {
	return map[string]any{"id": id, "parentId": nil, "author": author, "authorId": 1, "authorUsername": "u_" + author,
		"createdAt": ts(min), "updatedAt": nil, "body": body, "bodyHtml": "<p>" + body + "</p>"}
}

func root(id, author, body string, min int, replies ...map[string]any) map[string]any {
	m := c(id, author, body, min)
	m["pinned"] = false
	if replies == nil {
		replies = []map[string]any{}
	}
	m["replies"] = replies
	return m
}

func rep(id, parent, author, body string, min, depth int) map[string]any {
	m := c(id, author, body, min)
	m["parentId"], m["depth"] = parent, depth
	return m
}

type site struct {
	mu       sync.Mutex
	loggedIn bool
	cookies  bool
	pages    map[int][]map[string]any
	readErr  string // get_comments error code
}

func (s *site) install(f *mcptest.Server) {
	f.Handle("cf_session_status", func(map[string]any) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := map[string]any{"loggedIn": s.loggedIn, "cookiesStored": s.cookies, "detail": "d", "user": nil}
		if s.loggedIn {
			out["user"] = map[string]any{"id": 136845864, "displayName": "Morgott", "username": "user_x"}
		}
		return out, nil
	})
	f.Handle("search_author", func(args map[string]any) (any, error) {
		if args["username"] != "Morgott" {
			return nil, &mcptest.CodeError{Code: mcpbridge.CodeNotFound, Message: "no author"}
		}
		return map[string]any{"author": map[string]any{"id": 136845864, "username": "Morgott"},
			"projects": []any{map[string]any{"id": 1443010, "name": "Crossbow Save Arrow"}}}, nil
	})
	f.Handle("get_project", func(map[string]any) (any, error) {
		return map[string]any{"id": 1443010, "title": "Crossbow Save Arrow", "url": "https://www.curseforge.com/hytale/mods/crossbow-save-arrow"}, nil
	})
	f.Handle("get_comments", func(args map[string]any) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.readErr != "" {
			return nil, &mcptest.CodeError{Code: s.readErr, Message: "x"}
		}
		page := int(args["page"].(float64)) //nolint:forcetypeassert // test fixture
		total := 0
		for _, p := range s.pages {
			for _, t := range p {
				total += 1 + len(t["replies"].([]map[string]any)) //nolint:forcetypeassert // test fixture
			}
		}
		return map[string]any{"modId": 1443010, "page": page, "pages": len(s.pages), "pageSize": 20, "total": total,
			"comments": s.pages[page]}, nil
	})
}

type env struct {
	site *site
	fake *mcptest.Server
	prov *curseforge.Provider
	sy   *syncer.Syncer
	st   *store.Store
	evs  chan []store.Event
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{fake: mcptest.New(), evs: make(chan []store.Event, 8), site: &site{loggedIn: true, cookies: true,
		pages: map[int][]map[string]any{
			1: {root("20", "bob", "Not working", 20, rep("21", "20", "Morgott", "In reply to bob: which folder?", 21, 1),
				rep("22", "21", "bob", "earlyplugins", 22, 2))},
			2: {root("20", "bob", "Not working", 20), root("10", "alice", "Nice mod", 10)},
		}}}
	e.site.install(e.fake)
	b := mcpbridge.New(mcpbridge.Options{Name: "curseforge", Dial: e.fake.Dial, CallTimeout: 5 * time.Second})
	t.Cleanup(b.Close)
	e.prov = curseforge.New(curseforge.Options{Bridge: b, ReadBackWaits: []time.Duration{time.Millisecond},
		Now: func() time.Time { return time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC) }})
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e.st = store.New(db)
	e.sy = syncer.New(syncer.Options{Store: e.st, Provider: e.prov, OnUpdate: func(evs []store.Event, _ int) { e.evs <- evs }})
	return e
}

func TestSyncThreadsAllPages(t *testing.T) {
	e := setup(t)
	pr := provider.Project{ExternalID: "1443010", URL: "https://www.curseforge.com/hytale/mods/crossbow-save-arrow"}
	items, err := e.prov.SyncItems(t.Context(), pr, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items %+v", items)
	}
	got := map[string]provider.Item{}
	for _, it := range items {
		got[it.ExternalID] = it
	}
	th := got["comment:1443010/20"]
	if th.Kind != store.KindComment || th.Title != "Not working" || len(th.Comments) != 2 || th.Comments[0].Author != "Morgott" ||
		th.URL != "https://www.curseforge.com/hytale/mods/crossbow-save-arrow/comments" || !th.UpdatedAt.Equal(time.Date(2026, 9, 1, 10, 22, 0, 0, time.UTC)) {
		t.Fatalf("thread %+v", th)
	}
	if _, ok := got["comment:1443010/10"]; !ok {
		t.Fatal("page 2 thread missing")
	}

	if err := e.sy.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-e.evs
	if s := e.sy.Status(); !s.SignedIn || s.LastError != "" || s.Projects != 1 {
		t.Fatalf("status %+v", s)
	}
}

func TestSessionStates(t *testing.T) {
	e := setup(t)
	if a, err := e.prov.Account(t.Context()); err != nil || a != "Morgott" {
		t.Fatalf("account %q %v", a, err)
	}
	// Never signed in: signed out, not an error.
	e.site.loggedIn, e.site.cookies = false, false
	if _, err := e.prov.Account(t.Context()); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("no cookies: %v", err)
	}
	// Session expired: an error state (re-login), never "no new comments".
	e.site.cookies = true
	if _, err := e.prov.Account(t.Context()); !errors.Is(err, curseforge.ErrRelogin) || errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("expired: %v", err)
	}
	if err := e.sy.SyncOnce(t.Context()); err == nil {
		t.Fatal("expired session must fail the sync")
	}
	if s := e.sy.Status(); s.LastError == "" {
		t.Fatalf("status %+v", s)
	}
	// Reads refused mid-session (cookie expiry, Cloudflare) → re-login.
	e.site.loggedIn = true
	for _, code := range []string{mcpbridge.CodeNotLoggedIn, mcpbridge.CodeCloudflare} {
		e.site.readErr = code
		if _, err := e.prov.SyncItems(t.Context(), provider.Project{ExternalID: "1443010"}, time.Time{}); !errors.Is(err, curseforge.ErrRelogin) {
			t.Fatalf("%s: %v", code, err)
		}
	}
}

func TestDetectChangesReplyOnOldPage(t *testing.T) {
	e := setup(t)
	pr := provider.Project{ExternalID: "1443010"}
	var st provider.PollState
	if ch, err := e.prov.DetectChanges(t.Context(), pr, &st); err != nil || !ch.Overflow {
		t.Fatalf("first check must reconcile: %+v %v", ch, err)
	}
	if ch, err := e.prov.DetectChanges(t.Context(), pr, &st); err != nil || ch.Overflow {
		t.Fatalf("unchanged: %+v %v", ch, err)
	}
	e.site.mu.Lock()
	p2 := e.site.pages[2]
	p2[1]["replies"] = []map[string]any{rep("11", "10", "carol", "same", 50, 1)}
	e.site.mu.Unlock()
	if ch, _ := e.prov.DetectChanges(t.Context(), pr, &st); !ch.Overflow {
		t.Fatal("a reply on page 2 must change the fingerprint (total)")
	}
}

func TestReply(t *testing.T) {
	e := setup(t)
	e.fake.Handle("post_comment", func(args map[string]any) (any, error) {
		if args["mod_id"] != float64(1443010) || args["reply_to_id"] != float64(20) || args["comment_text"] != "try Mods" {
			t.Errorf("args %v", args)
		}
		return map[string]any{"posted": true, "id": "30", "parentId": "20", "verified": true}, nil
	})
	cm, err := e.prov.Reply(t.Context(), "comment:1443010/20", "try Mods")
	if err != nil || cm.ExternalID != "30" || cm.Author != "Morgott" {
		t.Fatalf("%+v %v", cm, err)
	}

	// Lost answer after the post: read back (the site prefixes "In reply to").
	e.fake.Handle("post_comment", func(map[string]any) (any, error) {
		e.site.mu.Lock()
		p1 := e.site.pages[1]
		p1[0]["replies"] = append(p1[0]["replies"].([]map[string]any), rep("31", "20", "Morgott", "In reply to bob: <b>fixed</b> now", 60, 1)) //nolint:forcetypeassert // fixture
		e.site.mu.Unlock()
		return nil, mcptest.ErrCrash
	})
	if cm, err = e.prov.Reply(t.Context(), "comment:1443010/20", "<b>fixed</b> now"); err != nil || cm.ExternalID != "31" {
		t.Fatalf("read-back: %+v %v", cm, err)
	}

	e.fake.Handle("post_comment", func(map[string]any) (any, error) {
		return map[string]any{"posted": true, "id": nil, "parentId": "20", "verified": false}, nil
	})
	if _, err = e.prov.Reply(t.Context(), "comment:1443010/20", "never shows"); !errors.Is(err, curseforge.ErrUnknownOutcome) {
		t.Fatalf("miss: %v", err)
	}
	e.fake.Handle("post_comment", func(map[string]any) (any, error) {
		return nil, &mcptest.CodeError{Code: mcpbridge.CodeNotLoggedIn, Message: "401"}
	})
	if _, err = e.prov.Reply(t.Context(), "comment:1443010/20", "x"); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("not logged in: %v", err)
	}
	if n := e.fake.Count("post_comment"); n != 4 {
		t.Fatalf("posts = %d (never retried)", n)
	}
}

// L6: the plain-text body is sent as escaped HTML with its line breaks.
func TestReplyBodyEscaped(t *testing.T) {
	e := setup(t)
	e.fake.Handle("post_comment", func(args map[string]any) (any, error) {
		if got := args["comment_text"]; got != "List&lt;String&gt; &amp; co<br>line 2" {
			t.Errorf("comment_text %q", got)
		}
		return map[string]any{"posted": true, "id": "40", "parentId": "20", "verified": true}, nil
	})
	if cm, err := e.prov.Reply(t.Context(), "comment:1443010/20", "List<String> & co\r\nline 2"); err != nil || cm.Body != "List<String> & co\r\nline 2" {
		t.Fatalf("%+v %v", cm, err)
	}
}

// H1: a write that failed after the send (outcome_unknown, unclassified) is
// read back; found → success, not found → unknown, never a plain failure.
// M4: an older identical reply is not taken for it.
func TestReplyUnsureWriteReadBack(t *testing.T) {
	e := setup(t)
	e.fake.Handle("post_comment", func(map[string]any) (any, error) {
		e.site.mu.Lock()
		p1 := e.site.pages[1]
		p1[0]["replies"] = append(p1[0]["replies"].([]map[string]any), rep("32", "20", "Morgott", "In reply to bob: line 1\nline <2>", 59, 1)) //nolint:forcetypeassert // fixture
		e.site.mu.Unlock()
		return nil, &mcptest.CodeError{Code: mcpbridge.CodeOutcomeUnknown, Message: "HTTP 502"}
	})
	if cm, err := e.prov.Reply(t.Context(), "comment:1443010/20", "line 1\nline <2>"); err != nil || cm.ExternalID != "32" {
		t.Fatalf("saved despite 502: %+v %v", cm, err)
	}
	for _, code := range []string{mcpbridge.CodeOutcomeUnknown, mcpbridge.CodeError} {
		e.fake.Handle("post_comment", func(map[string]any) (any, error) {
			return nil, &mcptest.CodeError{Code: code, Message: "HTTP 500"}
		})
		// "which folder?" already exists (id 21, before the post): not this reply.
		if _, err := e.prov.Reply(t.Context(), "comment:1443010/20", "which folder?"); !errors.Is(err, curseforge.ErrUnknownOutcome) {
			t.Fatalf("%s: %v", code, err)
		}
	}
	e.fake.Handle("post_comment", func(map[string]any) (any, error) {
		return nil, &mcptest.CodeError{Code: mcpbridge.CodeInvalid, Message: "bad"}
	})
	if _, err := e.prov.Reply(t.Context(), "comment:1443010/20", "x"); err == nil || errors.Is(err, curseforge.ErrUnknownOutcome) {
		t.Fatalf("refusal: %v", err)
	}
}

// L7: a cancelled request still reads the reply back.
func TestReplyReadBackSurvivesCancel(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	e.fake.Handle("post_comment", func(map[string]any) (any, error) {
		e.site.mu.Lock()
		p1 := e.site.pages[1]
		p1[0]["replies"] = append(p1[0]["replies"].([]map[string]any), rep("33", "20", "Morgott", "bye", 59, 1)) //nolint:forcetypeassert // fixture
		e.site.mu.Unlock()
		cancel()
		return nil, mcptest.ErrCrash
	})
	if cm, err := e.prov.Reply(ctx, "comment:1443010/20", "bye"); err != nil || cm.ExternalID != "33" {
		t.Fatalf("%+v %v", cm, err)
	}
}
