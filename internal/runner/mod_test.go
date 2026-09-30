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

// modItem adds the nexus mod page skyrim/7 named name with one comment item
// and returns the page's project id and the item id.
func (e *env) modItem(name string) (page, item int64) {
	e.t.Helper()
	ctx := e.t.Context()
	nx, err := e.st.UpsertSource(ctx, "nexus", "maintainer")
	if err != nil {
		e.t.Fatal(err)
	}
	mods, err := e.st.SyncProjects(ctx, nx, []provider.Project{{ExternalID: "skyrim/7", Name: name, URL: "https://www.nexusmods.com/skyrim/mods/7"}})
	if err != nil {
		e.t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := e.st.ApplyItems(ctx, nx, mods[0].ID, []provider.Item{{ExternalID: "comment:55", Kind: store.KindComment, Number: 1,
		Title: "Crashes on load", URL: "https://www.nexusmods.com/skyrim/mods/7?tab=posts#comment-55", Author: "player", Open: true, CreatedAt: now, UpdatedAt: now}}, "maintainer"); err != nil {
		e.t.Fatal(err)
	}
	chunk, err := e.st.Issues(ctx, store.IssueFilter{Platform: "nexus"})
	if err != nil || len(chunk.Items) != 1 {
		e.t.Fatalf("mod item: %+v %v", chunk, err)
	}
	return mods[0].ID, chunk.Items[0].ID
}

// A mod page fix changes the linked repo's code, so the repo's mode, verify
// and project notes decide, not the mod page's (absent) override.
func TestModFixFollowsCodeProjectSettings(t *testing.T) {
	mode(t, "ok")
	e := setup(t, 1, func(s *config.Settings) {
		s.Agents.Projects["github:octo/demo"] = config.ProjectAgent{Mode: config.ModeWorktreePR, Verify: "echo repo-verify", Prompt: "Repo notes {branch}."}
	})
	page, item := e.modItem("Demo Mod")
	if err := e.st.SetProjectLinks(t.Context(), e.proj[0].ID, []int64{page}); err != nil {
		t.Fatal(err)
	}
	if in, err := e.st.JobInput(t.Context(), item); err != nil || in.CodeKey != "github:octo/demo" || in.ProjectKey != "nexus:skyrim/7" {
		t.Fatalf("job input: %+v %v", in, err)
	}
	j := e.wait(e.enqueue(flowFix, item)[0].ID, store.JobNeedsReview)
	res := result(t, j)
	if res.Mode != config.ModeWorktreePR || j.Worktree == "" || !exists(j.Worktree) || res.Local != nil {
		t.Fatalf("mode: %+v %s", j, j.Result)
	}
	if res.Verify == nil || res.Verify.Command != "echo repo-verify" {
		t.Fatalf("verify: %+v", res.Verify)
	}
	sys, err := os.ReadFile(filepath.Join(jobDir(e.data, j.ID), "fix.system.md"))
	if err != nil || !strings.Contains(string(sys), "Repo notes "+j.Branch) {
		t.Fatalf("system prompt: %s %v", sys, err)
	}
}

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
	e.set(func(s *config.Settings) {
		s.Agents.Projects["nexus:skyrim/7"] = config.ProjectAgent{Mode: config.ModeDirect, ModPush: new(true)}
	})
	if j, err = e.r.Push(ctx, j.ID); err != nil || j.State != store.JobDone {
		t.Fatalf("allowed push: %+v %v", j, err)
	}
	if got := run(t, e.bare, "rev-parse", "main"); got != head {
		t.Fatalf("remote main %s, want %s", got, head)
	}
}
