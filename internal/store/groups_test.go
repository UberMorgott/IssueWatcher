package store

import (
	"errors"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// A code project and its linked mod pages are one project: one grouped row
// with summed counts and per-channel integrations, one issue scope, one
// triage, one sync target set; an unlinked mod page stays its own row.
func TestProjectGroups(t *testing.T) {
	ctx := t.Context()
	s := newStore(t)
	gh, err := s.UpsertSource(ctx, "github", "me")
	if err != nil {
		t.Fatal(err)
	}
	code, err := s.SyncProjects(ctx, gh, []provider.Project{{ExternalID: "o/app", Name: "o/app"}, {ExternalID: "o/lib", Name: "o/lib"}})
	if err != nil {
		t.Fatal(err)
	}
	nx, err := s.UpsertSource(ctx, "nexus", "me")
	if err != nil {
		t.Fatal(err)
	}
	mods, err := s.SyncProjects(ctx, nx, []provider.Project{{ExternalID: "skyrim/1", Name: "Mod One"}, {ExternalID: "skyrim/2", Name: "Mod Two"}})
	if err != nil {
		t.Fatal(err)
	}
	kind := func(it provider.Item, k string) provider.Item { it.Kind = k; return it }
	apply := func(src, project int64, items ...provider.Item) []Event {
		t.Helper()
		evs, err := s.ApplyItems(ctx, src, project, items, "me")
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
	app, mod1 := code[0].ID, mods[0].ID
	apply(gh, app, item("i1", 1, true, t0), item("i2", 2, false, t0))
	apply(nx, mod1, kind(item("bug:5", 5, true, t0), KindBug), kind(item("comment:6", 1, true, t0), KindComment))
	apply(nx, mods[1].ID, kind(item("comment:7", 7, true, t0), KindComment))
	if err := s.SetProjectLinks(ctx, app, []int64{mod1}); err != nil {
		t.Fatal(err)
	}
	// After the baseline: one new item each → unread, and the mod page's event names its code project.
	apply(gh, app, item("i3", 3, true, t0.Add(1)))
	evs := apply(nx, mod1, kind(item("bug:8", 8, true, t0.Add(1)), KindBug))
	if len(evs) != 1 || evs[0].Project != "nexus:skyrim/1" || evs[0].CodeProject != "github:o/app" {
		t.Fatalf("mod event: %+v", evs)
	}

	flat, err := s.ReposChunk(ctx, RepoQuery{Limit: 10})
	if err != nil || flat.Total != 4 || len(flat.Items) != 4 || flat.Items[0].Integrations != nil {
		t.Fatalf("flat list: %+v %v", flat, err)
	}
	g, err := s.ReposChunk(ctx, RepoQuery{Group: true, Sort: "name", Limit: 10})
	if err != nil || g.Total != 3 || len(g.Items) != 3 {
		t.Fatalf("grouped: %+v %v", g, err)
	}
	row := g.Items[1] // Mod Two, o/app, o/lib by name
	if g.Items[0].Name != "Mod Two" || row.Name != "o/app" || g.Items[2].Name != "o/lib" {
		t.Fatalf("grouped rows: %+v", g.Items)
	}
	if row.Open != 5 || row.Closed != 1 || row.Unread != 2 || len(row.Integrations) != 2 {
		t.Fatalf("grouped row: %+v", row)
	}
	own, m := row.Integrations[0], row.Integrations[1]
	if own.ID != app || own.Platform != "github" || own.Open != 2 || own.Closed != 1 || own.Unread != 1 ||
		m.ID != mod1 || m.Platform != "nexus" || m.Open != 3 || m.Unread != 1 {
		t.Fatalf("integrations: %+v", row.Integrations)
	}
	if lone := g.Items[0].Integrations; len(lone) != 1 || lone[0].ID != mods[1].ID {
		t.Fatalf("unlinked mod page: %+v", lone)
	}
	// Name filter over any member; sort by the summed unread.
	if f, err := s.ReposChunk(ctx, RepoQuery{Group: true, Text: "Mod One", Limit: 10}); err != nil || f.Total != 1 || len(f.Items) != 1 || f.Items[0].ID != app {
		t.Fatalf("filter by member: %+v %v", f, err)
	}
	if u, err := s.ReposChunk(ctx, RepoQuery{Group: true, Sort: "unread", Desc: true, Limit: 1}); err != nil || len(u.Items) != 1 || u.Items[0].ID != app || !u.More {
		t.Fatalf("sort by unread: %+v %v", u, err)
	}

	// Issue scope: the project's items include the mod page's; + platform narrows to one channel.
	for _, tc := range []struct {
		f    IssueFilter
		want int
	}{{IssueFilter{RepoID: app}, 6}, {IssueFilter{RepoID: app, Platform: "nexus"}, 3}, {IssueFilter{RepoID: app, Platform: "github"}, 3}, {IssueFilter{RepoID: mod1}, 3}} {
		c, err := s.Issues(ctx, tc.f)
		if err != nil || len(c.Items) != tc.want {
			t.Errorf("issues %+v: %d %v, want %d", tc.f, len(c.Items), err, tc.want)
		}
	}
	if st, err := s.Stats(ctx, app, 4, t0); err != nil || st.Open != 4 || st.Closed != 1 || st.OpenComments != 1 { // comment threads counted apart
		t.Fatalf("stats: %+v %v", st, err)
	}

	// Triage: own issues by number, mod reports by ModItemRef + item id.
	in, err := s.TriageInput(ctx, app, 50, 100)
	if err != nil || len(in.Issues) != 5 {
		t.Fatalf("triage input: %+v %v", in, err)
	}
	bug5 := itemID(t, s, "bug:5")
	var found bool
	for _, is := range in.Issues {
		if is.ItemID == bug5 {
			found = is.Number == ModItemRef+int(bug5) && is.Source == "nexus"
		} else if is.Source == "" && is.Number > 3 {
			t.Fatalf("own issue numbered %d", is.Number)
		}
	}
	if !found {
		t.Fatalf("mod report in triage: %+v", in.Issues)
	}
	open, err := s.OpenIssues(ctx, app, []int{1, 2, ModItemRef + int(bug5)})
	if err != nil || len(open) != 2 || open[ModItemRef+int(bug5)].ItemID != bug5 || open[1].Number != 1 {
		t.Fatalf("open issues: %+v %v", open, err)
	}

	// Sync targets: the project first, then its mod pages.
	ts, err := s.GroupTargets(ctx, app)
	if err != nil || len(ts) != 2 || ts[0].ID != app || ts[0].Platform != "github" || ts[1].ID != mod1 || ts[1].SourceID != nx {
		t.Fatalf("group targets: %+v %v", ts, err)
	}
	if _, err := s.GroupTargets(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

// One mod on several mod platforms without a repository (owner 2026-09-30): a
// mod page heads the group, the others are its members; no code folder is
// borrowed from a mod page head, and linking the head to a repository brings
// the whole group along (groups stay one level deep).
func TestModPageGroups(t *testing.T) {
	ctx := t.Context()
	s := newStore(t)
	src := func(platform string, ps ...provider.Project) []Project {
		t.Helper()
		id, err := s.UpsertSource(ctx, platform, "me")
		if err != nil {
			t.Fatal(err)
		}
		out, err := s.SyncProjects(ctx, id, ps)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range out {
			if _, err := s.ApplyItems(ctx, id, p.ID, []provider.Item{item("x:"+p.ExternalID, 1, true, t0)}, "me"); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	gh := src("github", provider.Project{ExternalID: "o/app", Name: "o/app", URL: "https://github.com/o/app"})[0]
	nx := src("nexus", provider.Project{ExternalID: "sky/1", Name: "Mod", URL: "https://nexus/1"}, provider.Project{ExternalID: "sky/2", Name: "Other"})
	st := src("steam", provider.Project{ExternalID: "77", Name: "Mod (Steam)", URL: "https://steam/77"})[0]

	if err := s.SetProjectLinks(ctx, nx[0].ID, []int64{st.ID}); err != nil {
		t.Fatalf("mod page head: %v", err)
	}
	g, err := s.ReposChunk(ctx, RepoQuery{Group: true, Sort: "name", Limit: 10})
	if err != nil || g.Total != 3 {
		t.Fatalf("grouped: %+v %v", g, err)
	}
	for _, r := range g.Items {
		if r.ID == st.ID {
			t.Fatalf("member listed as a row: %+v", g.Items)
		}
		if r.ID == nx[0].ID && (len(r.Integrations) != 2 || r.Integrations[1].ID != st.ID || r.Open != 2 || len(r.Links) != 1) {
			t.Fatalf("mod group row: %+v", r)
		}
	}
	if l, err := s.Links(ctx, st.ID); err != nil || l.LinkedTo == nil || l.LinkedTo.ID != nx[0].ID {
		t.Fatalf("member links: %+v %v", l, err)
	}
	// A hidden thread (only the owner wrote it) counts nowhere: not in the row, not in its channel.
	if _, err := s.db.ExecContext(ctx, `UPDATE items SET hidden = 1, status = 'closed' WHERE external_id = 'x:77'`); err != nil {
		t.Fatal(err)
	}
	if g, err := s.ReposChunk(ctx, RepoQuery{Group: true, Text: "Mod (Steam)", Limit: 10}); err != nil || len(g.Items) != 1 ||
		g.Items[0].Open != 1 || g.Items[0].Closed != 0 || g.Items[0].Integrations[1].Closed != 0 {
		t.Fatalf("hidden thread counted: %+v %v", g, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE items SET hidden = 0, status = 'open' WHERE external_id = 'x:77'`); err != nil {
		t.Fatal(err)
	}
	// No code project: the member still needs a repository for fixes.
	fx, err := s.fixTargets(ctx, []int64{st.ID})
	if err != nil || !fx[st.ID].NeedsLink || fx[st.ID].ProjectID != st.ID || fx[st.ID].Folder != "" {
		t.Fatalf("member fix target: %+v %v", fx, err)
	}
	in, err := s.JobInput(ctx, itemID(t, s, "x:77"))
	if err != nil || !in.Mod || in.CodeProject != "" || in.CodeRepo() != "" {
		t.Fatalf("member job input: %+v %v", in, err)
	}
	// One level deep: a member heads nothing, a repository joins nothing.
	if err := s.SetProjectLinks(ctx, st.ID, []int64{nx[1].ID}); !errors.Is(err, ErrBadLink) {
		t.Fatalf("member as head: %v", err)
	}
	if err := s.SetProjectLinks(ctx, nx[0].ID, []int64{st.ID, gh.ID}); !errors.Is(err, ErrBadLink) {
		t.Fatalf("repository as member: %v", err)
	}
	// Linking the head to the repository moves its members too.
	if err := s.SetProjectLinks(ctx, gh.ID, []int64{nx[0].ID}); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Links(ctx, gh.ID); err != nil || len(l.Links) != 2 {
		t.Fatalf("repository links: %+v %v", l, err)
	}
	if l, err := s.Links(ctx, st.ID); err != nil || l.LinkedTo == nil || l.LinkedTo.ID != gh.ID {
		t.Fatalf("moved member: %+v %v", l, err)
	}
	if in, err := s.JobInput(ctx, itemID(t, s, "x:77")); err != nil || in.CodeProject != "o/app" {
		t.Fatalf("moved member job input: %+v %v", in, err)
	}
}
