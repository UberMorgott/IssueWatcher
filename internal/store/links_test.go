package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Migration 009 adds project_links and keeps projects, items and folder
// mappings; a mod page's fixes use its linked code project's folder.
func TestMigration009ProjectLinks(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateTo(ctx, db, 8); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	gh, err := s.UpsertSource(ctx, "github", "me")
	if err != nil {
		t.Fatal(err)
	}
	code, err := s.SyncProjects(ctx, gh, []provider.Project{{ExternalID: "o/app", Name: "o/app", URL: "https://github.com/o/app"}, {ExternalID: "o/lib", Name: "o/lib", URL: "u2"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLocalPath(ctx, code[0].ID, `C:\src\app`); err != nil {
		t.Fatal(err)
	}
	nx, err := s.UpsertSource(ctx, "nexus", "me")
	if err != nil {
		t.Fatal(err)
	}
	mods, err := s.SyncProjects(ctx, nx, []provider.Project{{ExternalID: "skyrim/1", Name: "Mod One", URL: "https://nexusmods.com/skyrim/mods/1"}, {ExternalID: "skyrim/2", Name: "Mod Two"}})
	if err != nil {
		t.Fatal(err)
	}
	c := item("comment:9", 1, true, t0)
	c.Kind = KindComment
	if _, err := s.ApplyItems(ctx, nx, mods[0].ID, []provider.Item{c}, "me"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s = New(db)
	repos, err := s.Repos(ctx)
	if err != nil || len(repos) != 4 {
		t.Fatalf("projects kept: %+v %v", repos, err)
	}
	modItem := itemID(t, s, "comment:9")
	in, err := s.JobInput(ctx, modItem)
	if err != nil || !in.Mod || in.CodeProject != "" || in.LocalPath != "" || in.CodeRepo() != "" || in.ProjectKey != "nexus:skyrim/1" {
		t.Fatalf("unlinked mod input: %+v %v", in, err)
	}

	// Links: mod pages → a GitHub project only.
	for _, bad := range []struct{ code, mod int64 }{{mods[1].ID, mods[0].ID}, {code[0].ID, code[1].ID}} {
		if err := s.SetProjectLinks(ctx, bad.code, []int64{bad.mod}); !errors.Is(err, ErrBadLink) {
			t.Fatalf("link %v: %v", bad, err)
		}
	}
	if err := s.SetProjectLinks(ctx, code[0].ID, []int64{9999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link to no project: %v", err)
	}
	if err := s.SetProjectLinks(ctx, code[1].ID, []int64{mods[0].ID, mods[1].ID}); err != nil {
		t.Fatal(err)
	}
	// One code project per mod page: linking mod one to o/app moves it.
	if err := s.SetProjectLinks(ctx, code[0].ID, []int64{mods[0].ID}); err != nil {
		t.Fatal(err)
	}
	l, err := s.Links(ctx, code[0].ID)
	if err != nil || l.LinkedTo != nil || len(l.Links) != 1 || l.Links[0].ID != mods[0].ID || l.Links[0].Platform != "nexus" {
		t.Fatalf("code links: %+v %v", l, err)
	}
	if l, err := s.Links(ctx, code[1].ID); err != nil || len(l.Links) != 1 || l.Links[0].ID != mods[1].ID {
		t.Fatalf("other code links: %+v %v", l, err)
	}
	if l, err := s.Links(ctx, mods[0].ID); err != nil || l.LinkedTo == nil || l.LinkedTo.Name != "o/app" || len(l.Links) != 0 {
		t.Fatalf("mod links: %+v %v", l, err)
	}
	repos, _ = s.Repos(ctx)
	for _, r := range repos {
		switch r.ID {
		case code[0].ID:
			if r.LinkedTo != 0 || len(r.Links) != 1 || r.Links[0] != mods[0].ID {
				t.Fatalf("code row %+v", r)
			}
		case mods[0].ID:
			if r.LinkedTo != code[0].ID || len(r.Links) != 0 || r.Key != "nexus:skyrim/1" {
				t.Fatalf("mod row %+v", r)
			}
		}
	}
	chunk, err := s.ReposChunk(ctx, RepoQuery{Limit: 10})
	if err != nil || len(chunk.Items) != 4 {
		t.Fatalf("chunk %+v %v", chunk, err)
	}

	in, err = s.JobInput(ctx, modItem)
	if err != nil || !in.Mod || in.CodeProject != "o/app" || in.LocalPath != `C:\src\app` || in.ProjectURL != "https://github.com/o/app" ||
		in.CodeRepo() != "o/app" || in.ProjectName != "Mod One" {
		t.Fatalf("linked mod input: %+v %v", in, err)
	}
	if f, err := s.AutomationItemFacts(ctx, modItem); err != nil || f.LocalPath != `C:\src\app` || f.ProjectURL != "https://github.com/o/app" {
		t.Fatalf("facts: %+v %v", f, err)
	}
	j, err := s.CreateJob(ctx, modItem, "fix", "claude", "", "")
	if err != nil || !j.Mod || j.CodeProject != "o/app" || j.LocalPath != `C:\src\app` || j.Repo != "Mod One" {
		t.Fatalf("mod job: %+v %v", j, err)
	}

	if err := s.UnlinkProject(ctx, mods[0].ID); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Links(ctx, code[0].ID); err != nil || len(l.Links) != 0 {
		t.Fatalf("after unlink: %+v %v", l, err)
	}
	if err := s.UnlinkProject(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlink no project: %v", err)
	}
}
