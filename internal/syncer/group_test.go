package syncer

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// fakeProvider serves one project with one item; err fails every call.
type fakeProvider struct {
	platform, account, item string

	noReply  bool // Capabilities().Reply = false (Steam until a live post is verified)
	maxReply int  // Capabilities().MaxReply

	mu      sync.Mutex
	err     error
	replies []string
}

func (f *fakeProvider) fail() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeProvider) Platform() string { return f.platform }
func (f *fakeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Reply: !f.noReply, MaxReply: f.maxReply}
}
func (f *fakeProvider) Account(context.Context) (string, error) {
	if err := f.fail(); err != nil {
		return "", err
	}
	return f.account, nil
}

func (f *fakeProvider) ListProjects(context.Context) ([]provider.Project, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return []provider.Project{{ExternalID: "p", Name: f.platform + "/" + f.account}}, nil
}

func (f *fakeProvider) SyncItems(context.Context, provider.Project, time.Time) ([]provider.Item, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return []provider.Item{{ExternalID: "x1", Kind: "issue", Number: 1, Title: f.item, Author: "u", Open: true,
		CreatedAt: t0, UpdatedAt: t0}}, nil
}

func (f *fakeProvider) Reply(_ context.Context, id, body string) (provider.Comment, error) {
	if err := f.fail(); err != nil {
		return provider.Comment{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, id+":"+body)
	return provider.Comment{ExternalID: "r" + body, Author: f.account, Body: body, CreatedAt: t0}, nil
}

func TestGroupSourcesIndependentAndRepliesRouted(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	a := &fakeProvider{platform: "alpha", account: "a1", item: "item-a1"}
	c := &fakeProvider{platform: "alpha", account: "a2", item: "item-a2"} // second account, same platform
	b := &fakeProvider{platform: "beta", account: "b1", item: "item-b1", err: &provider.RateLimitError{Reset: t0.Add(time.Hour)}}
	var ss []*Syncer
	for _, p := range []*fakeProvider{a, b, c} {
		ss = append(ss, New(Options{Store: st, Provider: p, Log: slog.New(slog.DiscardHandler)}))
	}
	g := NewGroup(ss...)
	var (
		mu      sync.Mutex
		sources = map[string]bool{}
	)
	g.OnProgress(func(p Progress) { mu.Lock(); sources[p.Source] = true; mu.Unlock() })
	for _, s := range ss {
		_ = s.SyncOnce(t.Context())
	}
	gs := g.Status()
	if len(gs.Sources) != 3 || gs.LastSync == "" || gs.Sources[1].RateLimitedUntil == "" || gs.Sources[1].LastSync != "" ||
		gs.Sources[2].LastSync == "" || gs.Sources[2].Account != "a2" {
		t.Fatalf("status %+v", gs)
	}
	if !sources["alpha:a1"] || !sources["alpha:a2"] {
		t.Fatalf("progress sources %v", sources)
	}
	b.mu.Lock()
	b.err = nil
	b.mu.Unlock()
	if err := ss[1].SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	page, err := st.Issues(t.Context(), store.IssueFilter{})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("items %+v %v", page, err)
	}
	for _, it := range page.Items {
		if _, err := g.Reply(t.Context(), it.ID, it.Title); err != nil {
			t.Fatalf("reply %s: %v", it.Title, err)
		}
	}
	for _, p := range []*fakeProvider{a, b, c} {
		if len(p.replies) != 1 || p.replies[0] != "x1:"+p.item {
			t.Fatalf("%s replies %v", p.item, p.replies)
		}
	}
}

func TestGroupAddRemoveWhileRunning(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	log := slog.New(slog.DiscardHandler)
	g := NewGroup(New(Options{Store: st, Provider: &fakeProvider{platform: "alpha", account: "a1", item: "i"}, Log: log}))
	var mu sync.Mutex
	seen := map[string]bool{}
	g.OnProgress(func(p Progress) { mu.Lock(); seen[p.Source] = true; mu.Unlock() })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()

	late := New(Options{Store: st, Provider: &fakeProvider{platform: "beta", account: "b1", item: "j"}, Log: log})
	g.Add(late) // started at once, with the group's progress listener
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ok := seen["beta:b1"]
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a syncer added while running never reported progress")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := g.Remove("beta"); len(got) != 1 || got[0] != late {
		t.Fatalf("Remove(beta) = %v", got)
	}
	if n := len(g.Syncers()); n != 1 {
		t.Fatalf("syncers after remove = %d, want 1", n)
	}
	if got := g.Remove("alpha"); len(got) != 0 {
		t.Fatal("the primary syncer must never be removed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// M1: a platform without reply support never posts, whatever the path
// (Syncer.Reply for the API, Group.Reply for rule jobs and JobView send).
func TestReplyOffPlatformRefused(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	p := &fakeProvider{platform: "steam", account: "s1", item: "c", noReply: true}
	s := New(Options{Store: st, Provider: p, Log: slog.New(slog.DiscardHandler)})
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := st.Issues(t.Context(), store.IssueFilter{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("items %+v %v", page, err)
	}
	id := page.Items[0].ID
	if _, err := s.Reply(t.Context(), id, "hi"); !errors.Is(err, ErrReplyOff) {
		t.Fatalf("Syncer.Reply: %v", err)
	}
	if _, err := NewGroup(s).Reply(t.Context(), id, "hi"); !errors.Is(err, ErrReplyOff) {
		t.Fatalf("Group.Reply: %v", err)
	}
	if len(p.replies) != 0 {
		t.Fatalf("posted %v", p.replies)
	}
	// The fake reports no reply threads; a platform without a syncer counts as threaded.
	if g := NewGroup(s); g.ReplyThreaded("steam") || !g.ReplyThreaded("github") {
		t.Fatal("ReplyThreaded")
	}
	// The fake (steam) renders no Markdown; neither does a platform without a syncer.
	if g := NewGroup(s); g.ReplyMarkdown("steam") || g.ReplyMarkdown("github") {
		t.Fatal("ReplyMarkdown")
	}
}

// A reply longer than the platform accepts (Steam: under 1000 characters) is
// refused with ErrReplyTooLong before the platform is called.
func TestReplyTooLongRefused(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	p := &fakeProvider{platform: "steam", account: "s1", item: "c", maxReply: 999}
	s := New(Options{Store: st, Provider: p, Log: slog.New(slog.DiscardHandler)})
	if err := s.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := st.Issues(t.Context(), store.IssueFilter{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("items %+v %v", page, err)
	}
	id := page.Items[0].ID
	if _, err := NewGroup(s).Reply(t.Context(), id, strings.Repeat("я", 1000)); !errors.Is(err, provider.ErrReplyTooLong) {
		t.Fatalf("1000 characters: %v", err)
	}
	if len(p.replies) != 0 {
		t.Fatalf("posted %v", p.replies)
	}
	if _, err := NewGroup(s).Reply(t.Context(), id, strings.Repeat("я", 999)); err != nil {
		t.Fatalf("999 characters: %v", err)
	}
	if g := NewGroup(s); g.MaxReply("steam") != 999 || g.MaxReply("github") != 0 {
		t.Fatal("MaxReply")
	}
}

// A project's row sync re-reads the project and its linked mod pages, each
// through its own source; a failing source reports its own error (relogin).
func TestGroupSyncProjects(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	gh := &fakeProvider{platform: "github", account: "me", item: "code"}
	nx := &fakeProvider{platform: "nexus", account: "me", item: "mod"}
	sg := New(Options{Store: st, Provider: gh})
	sn := New(Options{Store: st, Provider: nx})
	g := NewGroup(sg, sn)
	for _, s := range []*Syncer{sg, sn} {
		if err := s.SyncOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	repos, err := st.Repos(t.Context())
	if err != nil || len(repos) != 2 {
		t.Fatalf("repos: %+v %v", repos, err)
	}
	ids := map[string]int64{}
	for _, r := range repos {
		ids[r.Platform] = r.ID
	}
	if err := st.SetProjectLinks(t.Context(), ids["github"], []int64{ids["nexus"]}); err != nil {
		t.Fatal(err)
	}
	done := make(chan Progress, 8)
	g.OnProgress(func(p Progress) {
		if p.State == ProgressDone || p.State == ProgressError {
			done <- p
		}
	})
	targets, err := st.GroupTargets(t.Context(), ids["github"])
	if err != nil {
		t.Fatal(err)
	}
	wait := func() map[string]Progress {
		t.Helper()
		got := map[string]Progress{}
		for range 2 {
			select {
			case p := <-done:
				got[p.Source] = p
			case <-time.After(5 * time.Second):
				t.Fatalf("project sync: only %+v", got)
			}
		}
		return got
	}
	if started, missing := g.SyncProjects(t.Context(), targets); len(started) != 2 || len(missing) != 0 {
		t.Fatalf("started %v missing %v", started, missing)
	}
	got := wait()
	if p := got["github:me"]; p.State != ProgressDone || p.Repo != "github/me" || got["nexus:me"].State != ProgressDone {
		t.Fatalf("progress: %+v", got)
	}

	nx.mu.Lock()
	nx.err = provider.ErrRelogin
	nx.mu.Unlock()
	g.SyncProjects(t.Context(), targets)
	got = wait()
	if got["nexus:me"].State != ProgressError || !sn.Status().Relogin || got["github:me"].State != ProgressDone || sg.Status().Relogin {
		t.Fatalf("relogin: %+v", got)
	}
	if _, missing := g.SyncProjects(t.Context(), []store.SyncTarget{{Platform: "steam"}}); len(missing) != 1 {
		t.Fatalf("no steam account: %v", missing)
	}
}

// After an account switch (A -> B) the old account's projects stay active, but
// a row sync of one never writes the new account's items into it.
func TestGroupSyncProjectsAccountSwitch(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	nx := &fakeProvider{platform: "nexus", account: "A", item: "a-item"}
	sn := New(Options{Store: st, Provider: nx, Log: slog.New(slog.DiscardHandler)})
	g := NewGroup(sn)
	if err := sn.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	repos, err := st.Repos(t.Context())
	if err != nil || len(repos) != 1 {
		t.Fatalf("repos: %+v %v", repos, err)
	}
	projA := repos[0].ID
	nx.account, nx.item = "B", "b-item"
	if err := sn.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	targets, err := st.GroupTargets(t.Context(), projA)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %+v %v", targets, err)
	}
	if targets[0].SourceID == sn.sourceID() {
		t.Fatalf("project A reported under source B")
	}
	if started, missing := g.SyncProjects(t.Context(), targets); len(started) != 0 || len(missing) != 1 {
		t.Fatalf("A's project synced through B: started %v missing %v", started, missing)
	}
	if err := sn.SyncProject(t.Context(), targets[0].SourceID, targets[0].Project); !errors.Is(err, ErrNoSource) {
		t.Fatalf("direct sync of A's project through B: %v", err)
	}
}

// blockingProvider holds SyncItems until release closes (an old engine's
// request already on the wire when its platform is switched).
type blockingProvider struct {
	*fakeProvider
	entered chan struct{}
	release chan struct{}
}

func (b *blockingProvider) SyncItems(ctx context.Context, p provider.Project, since time.Time) ([]provider.Item, error) {
	close(b.entered)
	<-b.release
	items, err := b.fakeProvider.SyncItems(ctx, p, since)
	for i := range items {
		items[i].UpdatedAt = t0.Add(time.Hour) // newer: a write would overwrite
	}
	return items, err
}

// F3: a row sync still running when its syncer is removed (engine switch,
// platform off) never writes: the new source's items stay.
func TestRemovedSyncerProjectSyncWritesNothing(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := store.New(db)
	log := slog.New(slog.DiscardHandler)
	nx := &fakeProvider{platform: "nexus", account: "me", item: "fresh"}
	old := New(Options{Store: st, Provider: nx, Log: log})
	if err := old.SyncOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	g := NewGroup(New(Options{Store: st, Provider: &fakeProvider{platform: "github", account: "me", item: "code"}, Log: log}), old)
	repos, err := st.Repos(t.Context())
	if err != nil || len(repos) != 1 {
		t.Fatalf("repos: %+v %v", repos, err)
	}
	targets, err := st.GroupTargets(t.Context(), repos[0].ID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %+v %v", targets, err)
	}
	bp := &blockingProvider{fakeProvider: &fakeProvider{platform: "nexus", account: "me", item: "stale"},
		entered: make(chan struct{}), release: make(chan struct{})}
	old.opts.Provider = bp // the old engine's request is slow
	done := make(chan Progress, 1)
	g.OnProgress(func(p Progress) {
		if p.State == ProgressDone || p.State == ProgressError {
			done <- p
		}
	})
	if started, _ := g.SyncProjects(t.Context(), targets); len(started) != 1 {
		t.Fatalf("started %v", started)
	}
	<-bp.entered
	g.Remove("nexus") // the engine switch
	close(bp.release)
	select {
	case p := <-done:
		if p.State != ProgressError {
			t.Fatalf("removed syncer's sync reported %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("row sync never ended")
	}
	page, err := st.Issues(t.Context(), store.IssueFilter{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Title != "fresh" {
		t.Fatalf("items after removal: %+v %v", page.Items, err)
	}
}
