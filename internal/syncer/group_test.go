package syncer

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// fakeProvider serves one project with one item; err fails every call.
type fakeProvider struct {
	platform, account, item string

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
	return provider.Capabilities{Reply: true}
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
