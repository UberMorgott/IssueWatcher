package store

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

func TestMatchCode(t *testing.T) {
	codes := []Repo{
		{ID: 1, Name: "UberMorgott/crossbow-save-arrow", Platform: CodePlatform},
		{ID: 2, Name: "UberMorgott/ShareShip", Platform: CodePlatform},
		{ID: 3, Name: "someone/shareship", Platform: CodePlatform},
		{ID: 4, Name: "UberMorgott/WindroseTweaks", Platform: CodePlatform},
	}
	for _, c := range []struct {
		mod          Repo
		exact, loose []int64
	}{
		{Repo{Name: "Crossbow Save Arrow", URL: "https://www.curseforge.com/hytale/mods/crossbow-save-arrow"}, []int64{1}, nil},
		{Repo{Name: "Arrow saver", URL: "https://www.curseforge.com/hytale/mods/crossbow-save-arrow"}, []int64{1}, nil}, // slug
		{Repo{Name: "ShareShip", URL: "https://www.nexusmods.com/windrose/mods/147"}, []int64{2, 3}, nil},               // ambiguous
		{Repo{Name: "Windrose Tweaks Extended"}, nil, []int64{4}},                                                       // partial
		{Repo{Name: "Oracle", URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=3739613434"}, nil, nil},
	} {
		exact, loose := matchCode(c.mod, codes)
		if !slices.Equal(exact, c.exact) || !slices.Equal(loose, c.loose) {
			t.Errorf("%q: exact %v loose %v, want %v %v", c.mod.Name, exact, loose, c.exact, c.loose)
		}
	}
}

// Confident matches link themselves once; ambiguous ones are suggestions; a
// link removed by hand is never made again.
func TestAutoLink(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(db)
	gh, _ := s.UpsertSource(ctx, "github", "me")
	code, err := s.SyncProjects(ctx, gh, []provider.Project{
		{ExternalID: "me/crossbow-save-arrow", Name: "me/crossbow-save-arrow", URL: "https://github.com/me/crossbow-save-arrow"},
		{ExternalID: "me/shareship", Name: "me/shareship", URL: "https://github.com/me/shareship"},
		{ExternalID: "fork/shareship", Name: "fork/shareship", URL: "https://github.com/fork/shareship"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := s.UpsertSource(ctx, "curseforge", "me")
	nx, _ := s.UpsertSource(ctx, "nexus", "me")
	cfMods, err := s.SyncProjects(ctx, cf, []provider.Project{{ExternalID: "1443010", Name: "Crossbow Save Arrow", URL: "https://www.curseforge.com/hytale/mods/crossbow-save-arrow"}})
	if err != nil {
		t.Fatal(err)
	}
	nxMods, err := s.SyncProjects(ctx, nx, []provider.Project{{ExternalID: "windrose/147", Name: "ShareShip", URL: "https://www.nexusmods.com/windrose/mods/147"}})
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.AutoLink(ctx)
	if err != nil || n != 1 {
		t.Fatalf("auto-linked %d %v", n, err)
	}
	l, _ := s.Links(ctx, cfMods[0].ID)
	if l.LinkedTo == nil || l.LinkedTo.ID != code[0].ID {
		t.Fatalf("cf link %+v", l)
	}
	repos, err := s.Repos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		want := []int64(nil)
		if r.ID == nxMods[0].ID {
			want = []int64{code[2].ID, code[1].ID} // by name: fork/ first
		}
		if !slices.Equal(r.Suggest, want) {
			t.Fatalf("%s suggest %v, want %v", r.Name, r.Suggest, want)
		}
	}
	// Unlinked by hand: stays unlinked, no suggestion either.
	if err := s.UnlinkProject(ctx, cfMods[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.AutoLink(ctx); err != nil || n != 0 {
		t.Fatalf("relinked %d %v", n, err)
	}
	// Picking a suggestion is a decision: the rest of it disappears.
	if err := s.SetProjectLinks(ctx, code[1].ID, []int64{nxMods[0].ID}); err != nil {
		t.Fatal(err)
	}
	repos, _ = s.Repos(ctx)
	for _, r := range repos {
		if len(r.Suggest) > 0 {
			t.Fatalf("%s still suggests %v", r.Name, r.Suggest)
		}
	}
}
