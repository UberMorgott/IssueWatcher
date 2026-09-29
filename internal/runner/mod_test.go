package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// A fix of a mod-page item runs in the linked code project's folder, its
// prompt never asks for "Fixes #N", and Push stays off unless agents.modPush
// allows it for that mod page.
func TestModFixRunsInCodeFolder(t *testing.T) {
	mode(t, "nofixes")
	e := setup(t, 1, nil)
	ctx := t.Context()
	nx, err := e.st.UpsertSource(ctx, "nexus", "maintainer")
	if err != nil {
		t.Fatal(err)
	}
	mods, err := e.st.SyncProjects(ctx, nx, []provider.Project{{ExternalID: "skyrim/7", Name: "Demo Mod", URL: "https://www.nexusmods.com/skyrim/mods/7"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	const reportURL = "https://www.nexusmods.com/skyrim/mods/7?tab=posts#comment-55"
	if _, err := e.st.ApplyItems(ctx, nx, mods[0].ID, []provider.Item{{ExternalID: "comment:55", Kind: store.KindComment, Number: 1,
		Title: "Crashes on load", Body: "CTD when loading a save", URL: reportURL, Author: "player", Open: true, CreatedAt: now, UpdatedAt: now}}, "maintainer"); err != nil {
		t.Fatal(err)
	}
	var modItem int64
	chunk, err := e.st.Issues(ctx, store.IssueFilter{Platform: "nexus"})
	if err != nil || len(chunk.Items) != 1 {
		t.Fatalf("mod item: %+v %v", chunk, err)
	}
	modItem = chunk.Items[0].ID
	if f := chunk.Items[0].Fix; f.Fixable || !f.NeedsLink || f.Folder != "" || f.ProjectID != mods[0].ID {
		t.Fatalf("unlinked item fix: %+v", f)
	}

	// Not linked yet: no folder to fix in; the hint says to link the mod.
	if q, err := e.r.Enqueue(ctx, []int64{modItem}, flowFix, ""); !errors.Is(err, ErrNoFolder) || q[0].Error != CodeNoFolder || q[0].Hint != HintLinkMod {
		t.Fatalf("unlinked fix: %+v %v", q, err)
	}
	if err := e.st.SetProjectLinks(ctx, e.proj[0].ID, []int64{mods[0].ID}); err != nil {
		t.Fatal(err)
	}
	// Linked: the item and its mod page report the code project's folder as fixable.
	if chunk, err = e.st.Issues(ctx, store.IssueFilter{Platform: "nexus"}); err != nil || len(chunk.Items) != 1 {
		t.Fatalf("mod item: %+v %v", chunk, err)
	}
	if f := chunk.Items[0].Fix; !f.Fixable || f.NeedsLink || f.Folder != e.local || f.ProjectID != e.proj[0].ID {
		t.Fatalf("linked item fix: %+v (want %s)", f, e.local)
	}
	repos, err := e.st.Repos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rp := range repos {
		if rp.ID == mods[0].ID && (!rp.Fixable || rp.Folder != e.local || rp.ProjectID != e.proj[0].ID) {
			t.Fatalf("linked mod page fix: %+v", rp.Fix)
		}
	}
	j := e.wait(e.enqueue(flowFix, modItem)[0].ID, store.JobNeedsReview)
	res := result(t, j)
	if !j.Mod || j.CodeProject != "octo/demo" || j.LocalPath != e.local || res.Local == nil || res.Local.Dir != e.local ||
		len(res.Local.Commits) != 1 || res.Local.FixesRef || res.Local.Outcome != OutcomeFixedLocal {
		t.Fatalf("mod fix: %+v %s", j, j.Result)
	}
	sys, _ := os.ReadFile(filepath.Join(jobDir(e.data, j.ID), "fix-direct.system.md"))
	if strings.Contains(string(sys), "Fixes #1") || !strings.Contains(string(sys), `"Reported on `+reportURL+`"`) ||
		!strings.Contains(string(sys), "mod page Demo Mod on nexus") || !strings.Contains(string(sys), "Never write \"Fixes #N\"") {
		t.Fatalf("mod prompt:\n%s", sys)
	}

	// Push / PR are off for mod items by default; the commit stays local.
	if _, err := e.r.Push(ctx, j.ID); !errors.Is(err, ErrModItem) {
		t.Fatalf("push: %v", err)
	}
	if _, err := e.r.CreatePR(ctx, j.ID); !errors.Is(err, ErrModItem) {
		t.Fatalf("pr: %v", err)
	}
	head := run(t, e.local, "rev-parse", "HEAD")
	if got := run(t, e.bare, "rev-parse", "main"); got == head {
		t.Fatal("pushed while off")
	}
	// The mod page's override allows it: the commit reaches the code repo.
	e.set(func(s *config.Settings) { s.Agents.Projects["nexus:skyrim/7"] = config.ProjectAgent{Mode: config.ModeDirect, ModPush: new(true)} })
	if j, err = e.r.Push(ctx, j.ID); err != nil || j.State != store.JobDone {
		t.Fatalf("allowed push: %+v %v", j, err)
	}
	if got := run(t, e.bare, "rev-parse", "main"); got != head {
		t.Fatalf("remote main %s, want %s", got, head)
	}
}
