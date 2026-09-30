package store

import (
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// The Issues and Comments pages: a kind list filters the list, and project
// counts split unread comments from unread issues and bug reports.
func TestIssuesKindsAndUnreadComments(t *testing.T) {
	s, src, p := setup(t)
	ctx := t.Context()
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{item("I1", 1, true, t0)}, "me"); err != nil {
		t.Fatal(err)
	}
	withKind := func(it provider.Item, kind string) provider.Item {
		it.Kind = kind
		return it
	}
	later := t0.Add(time.Hour)
	if _, err := s.ApplyItems(ctx, src, p.ID, []provider.Item{
		item("I1", 1, true, t0),
		item("I2", 2, true, later),
		withKind(item("C1", 3, true, later), KindComment),
		withKind(item("B1", 4, true, later), KindBug),
	}, "me"); err != nil {
		t.Fatal(err)
	}

	count := func(kinds ...string) int {
		t.Helper()
		c, err := s.Issues(ctx, IssueFilter{Kinds: kinds})
		if err != nil {
			t.Fatal(err)
		}
		return len(c.Items)
	}
	if n := count(KindIssue, KindBug); n != 3 {
		t.Fatalf("issue,bug: %d items, want 3", n)
	}
	if n := count(KindComment); n != 1 {
		t.Fatalf("comment: %d items, want 1", n)
	}
	if n := count(); n != 4 {
		t.Fatalf("all: %d items, want 4", n)
	}

	repos, err := s.Repos(ctx)
	if err != nil || len(repos) != 1 || repos[0].Unread != 3 || repos[0].UnreadComments != 1 || repos[0].OpenComments != 1 {
		t.Fatalf("repos %+v %v", repos, err)
	}
	for _, group := range []bool{false, true} {
		c, err := s.ReposChunk(ctx, RepoQuery{Group: group})
		if err != nil || len(c.Items) != 1 || c.Items[0].Unread != 3 || c.Items[0].UnreadComments != 1 || c.Items[0].OpenComments != 1 {
			t.Fatalf("chunk group=%v %+v %v", group, c.Items, err)
		}
		if group && (len(c.Items[0].Integrations) != 1 || c.Items[0].Integrations[0].UnreadComments != 1 || c.Items[0].Integrations[0].OpenComments != 1) {
			t.Fatalf("integrations %+v", c.Items[0].Integrations)
		}
	}
}
