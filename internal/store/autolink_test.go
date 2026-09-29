package store

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// The owner's mod pages (v0.6.0 only suggested these) link themselves; an
// ambiguous name stays a suggestion.
func TestLinkPlan(t *testing.T) {
	code := func(id int64, name string) Repo {
		return Repo{ID: id, Name: "UberMorgott/" + name, URL: "https://github.com/UberMorgott/" + name, Platform: CodePlatform}
	}
	steam := func(id int64, name, file string) Repo {
		return Repo{ID: id, Name: name, Platform: "steam", URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=" + file}
	}
	repos := []Repo{
		code(1, "Hytale-Mod-CrossbowSaveArrow"), code(2, "Hytale-Mod-NickNameChanger"), code(3, "Hytale-Mod-UltimateSaver"),
		code(4, "PhoenixPoint-Mod-FreeCamera"), code(5, "PhoenixPoint-Mod-PerkOracle"), code(6, "PhoenixPoint-Mod-Renderforge"),
		code(7, "ShareShip"), code(8, "ShareMap"), code(9, "PhoenixPoint-Mod-PPCli"), code(10, "Valheim-Mod-Auga-Fork"),
		code(11, "Wartales-Mod-Remastered-Fork"), code(12, "Hytale-Mod-TwinTools"), code(13, "Other-Mod-TwinTools"),
		code(14, "Valheim-Mod-SmoothRegen"),
		{ID: 20, Name: "Crossbow Save Arrow", Platform: "curseforge", URL: "https://www.curseforge.com/hytale/mods/crossbow-save-arrow"},
		{ID: 21, Name: "Nick Name Changer", Platform: "curseforge", URL: "https://www.curseforge.com/hytale/mods/nick-name-changer"},
		{ID: 22, Name: "Ultimate Saver", Platform: "curseforge", URL: "https://www.curseforge.com/hytale/mods/ultimate-saver"},
		steam(23, "Free Camera", "3752059126"), steam(24, "Oracle", "3739613434"), steam(25, "Renderforge", "3796708495"),
		{ID: 26, Name: "ShareShip - Open Any Ship's Management UI", Platform: "nexus", URL: "https://www.nexusmods.com/windrose/mods/147"},
		{ID: 27, Name: "Auga", Platform: "thunderstore", URL: "https://thunderstore.io/c/valheim/p/x/Auga/"},        // -Fork suffix
		{ID: 28, Name: "Twin Tools", Platform: "nexus", URL: "https://www.nexusmods.com/skyrim/mods/9"},             // same core twice, no game match
		{ID: 29, Name: "Regen", Platform: "steam", URL: "https://steamcommunity.com/sharedfiles/filedetails/?id=9"}, // word match, game unknown
		{ID: 30, Name: "Linked by text", Platform: "curseforge", URL: "https://www.curseforge.com/hytale/mods/xyz"},
	}
	hints := map[int64]linkHint{23: {game: "steam:839770"}, 24: {game: "steam:839770"}, 25: {game: "steam:839770"},
		30: {codeURL: "https://github.com/ubermorgott/ShareMap"}}
	auto, suggest := linkPlan(repos, nil, hints)
	want := map[int64]int64{20: 1, 21: 2, 22: 3, 23: 4, 24: 5, 25: 6, 26: 7, 27: 10, 30: 8}
	if len(auto) != len(want) {
		t.Errorf("auto %v, want %v", auto, want)
	}
	for mod, c := range want {
		if auto[mod] != c {
			t.Errorf("mod %d → %d, want %d", mod, auto[mod], c)
		}
	}
	if !slices.Equal(suggest[28], []int64{12, 13}) || !slices.Equal(suggest[29], []int64{14}) || len(suggest) != 2 {
		t.Errorf("suggest %v", suggest)
	}
	// Oracle without the app's game learned from its sibling pages: suggestion only.
	solo := slices.DeleteFunc(slices.Clone(repos), func(r Repo) bool { return r.ID == 23 || r.ID == 25 })
	auto, suggest = linkPlan(solo, nil, hints)
	if _, ok := auto[24]; ok || !slices.Equal(suggest[24], []int64{5}) {
		t.Errorf("solo Oracle: auto %v suggest %v", auto, suggest)
	}
	// Decided and linked pages are left alone; a linked page still teaches its game.
	solo[slices.IndexFunc(solo, func(r Repo) bool { return r.ID == 20 })].LinkedTo = 99
	auto, _ = linkPlan(append(solo, Repo{ID: 31, Name: "Free Camera", Platform: "steam", LinkedTo: 4}), map[int64]bool{21: true}, map[int64]linkHint{24: {game: "steam:839770"}, 31: {game: "steam:839770"}})
	if _, ok := auto[21]; ok || auto[20] != 0 || auto[24] != 5 {
		t.Errorf("decided/linked: %v", auto)
	}
}

// A single exact name match of another game is a suggestion, not a link; a
// game extending the prefix's still links.
func TestLinkPlanOtherGame(t *testing.T) {
	repos := []Repo{
		{ID: 1, Name: "me/Fallout4-Mod-FreeCamera", Platform: CodePlatform},
		{ID: 2, Name: "me/Skyrim-Mod-Lanterns", Platform: CodePlatform},
		{ID: 10, Name: "Free Camera", Platform: "nexus", URL: "https://www.nexusmods.com/skyrim/mods/9"},
		{ID: 11, Name: "Free Camera", Platform: "nexus", URL: "https://www.nexusmods.com/fallout4/mods/9"},
		{ID: 12, Name: "Lanterns", Platform: "nexus", URL: "https://www.nexusmods.com/skyrimspecialedition/mods/5"},
	}
	auto, suggest := linkPlan(repos, nil, nil)
	if _, ok := auto[10]; ok || auto[11] != 1 || auto[12] != 2 || !slices.Equal(suggest[10], []int64{1}) {
		t.Errorf("auto %v suggest %v", auto, suggest)
	}
}

// An unlink made by hand after the linker read its plan wins over the plan.
func TestAutoLinkUnlinkedMeanwhile(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(db)
	gh, _ := s.UpsertSource(ctx, "github", "me")
	code, err := s.SyncProjects(ctx, gh, []provider.Project{{ExternalID: "me/shareship", Name: "me/shareship"}})
	if err != nil {
		t.Fatal(err)
	}
	nx, _ := s.UpsertSource(ctx, "nexus", "me")
	mods, err := s.SyncProjects(ctx, nx, []provider.Project{{ExternalID: "windrose/147", Name: "ShareShip", URL: "https://www.nexusmods.com/windrose/mods/147"}})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := s.Repos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	auto, _, err := s.plan(ctx, repos)
	if err != nil || auto[mods[0].ID] != code[0].ID {
		t.Fatalf("plan %v %v", auto, err)
	}
	if err := s.UnlinkProject(ctx, mods[0].ID); err != nil { // the user decides between plan and write
		t.Fatal(err)
	}
	if n, err := s.applyAutoLinks(ctx, auto); err != nil || n != 0 {
		t.Fatalf("auto-linked %d %v", n, err)
	}
	if l, _ := s.Links(ctx, mods[0].ID); l.LinkedTo != nil {
		t.Fatalf("unlink reverted: %+v", l)
	}
}

func TestSplitWords(t *testing.T) {
	for in, want := range map[string][]string{"PerkOracle": {"perk", "oracle"}, "PPCli_v2": {"pp", "cli", "v", "2"}, "wartales-mp": {"wartales", "mp"}} {
		if got := splitWords(in); !slices.Equal(got, want) {
			t.Errorf("%s: %v", in, got)
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

// Provider hints (Steam app, description link) are stored and drive the linker.
func TestAutoLinkHints(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New(db)
	gh, _ := s.UpsertSource(ctx, "github", "me")
	code, err := s.SyncProjects(ctx, gh, []provider.Project{
		{ExternalID: "me/PhoenixPoint-Mod-FreeCamera", Name: "me/PhoenixPoint-Mod-FreeCamera"},
		{ExternalID: "me/PhoenixPoint-Mod-PerkOracle", Name: "me/PhoenixPoint-Mod-PerkOracle"},
		{ExternalID: "me/Tools", Name: "me/Tools"},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.UpsertSource(ctx, "steam", "me")
	mods := []provider.Project{
		{ExternalID: "1", Name: "Free Camera", Game: "steam:839770"},
		{ExternalID: "2", Name: "Oracle", Game: "steam:839770"},
		{ExternalID: "3", Name: "Helper", Game: "steam:839770", CodeURL: "https://github.com/me/tools"},
	}
	if _, err := s.SyncProjects(ctx, st, mods); err != nil {
		t.Fatal(err)
	}
	// A later sync without the hints (details lookup failed) keeps them.
	for i := range mods {
		mods[i].Game, mods[i].CodeURL = "", ""
	}
	sm, err := s.SyncProjects(ctx, st, mods)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.AutoLink(ctx); err != nil || n != 3 {
		t.Fatalf("auto-linked %d %v", n, err)
	}
	for i, c := range []int{0, 1, 2} {
		if l, _ := s.Links(ctx, sm[i].ID); l.LinkedTo == nil || l.LinkedTo.ID != code[c].ID {
			t.Errorf("%s → %+v", sm[i].Name, l.LinkedTo)
		}
	}
}
