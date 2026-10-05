package release

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Gaps of the first live release run: placeholders in the build and verify
// commands, and a folder ahead of the remote pushed by the run.

func TestExpandCommand(t *testing.T) {
	got, err := expandCommand(`pwsh -c "& { zip {name}_{version}.zip }" {version}`, "my-mod", "1.2.3-rc.1+b5")
	if err != nil || got != `pwsh -c "& { zip my-mod_1.2.3-rc.1+b5.zip }" 1.2.3-rc.1+b5` {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := expandCommand("build.cmd", "", ""); err != nil || got != "build.cmd" {
		t.Fatalf("no placeholders: %q %v", got, err)
	}
	for _, bad := range []string{"", "1.0&calc", `1.0" x`, "a b", "-x", "1;rm"} {
		if _, err := expandCommand("x {version}", "n", bad); err == nil {
			t.Errorf("version %q accepted", bad)
		}
	}
	if _, err := expandCommand("x {name}", "my mod", "1.0.0"); err == nil {
		t.Error("name with a space accepted")
	}
}

func TestReleaseExpandsBuildAndVerifyPlaceholders(t *testing.T) {
	e := newEnv(t)
	pa := e.cfg.Agents.Projects[codeKey]
	pa.PublishProfile.Build.Command = "build.cmd {name} {version}"
	pa.Verify = "check {name} {version}"
	e.cfg.Agents.Projects[codeKey] = pa
	e.verify = pa.Verify
	en := e.engine(false)
	p, err := en.Plan(t.Context(), e.codeID, Request{})
	if err != nil || !p.OK {
		t.Fatalf("plan %v %+v", err, p.Refusals)
	}
	if !slices.ContainsFunc(p.Steps, func(s PlanStep) bool { return strings.Contains(s.Request, "build.cmd my-mod 1.0.1") }) {
		t.Fatalf("plan steps %+v", p.Steps)
	}
	r := e.run(e.release(en, Request{}).ID)
	if r.State != store.RunDone {
		t.Fatalf("run %+v", r)
	}
	if !slices.Equal(e.commands, []string{"build.cmd my-mod 1.0.1"}) || !slices.Equal(e.verified, []string{"check my-mod 1.0.1"}) {
		t.Fatalf("build %q verify %q", e.commands, e.verified)
	}

	// An unsafe value is refused up front, never quoted into the command.
	e2 := newEnv(t)
	pa = e2.cfg.Agents.Projects[codeKey]
	pa.PublishProfile.Build.Command = "build.cmd {name}"
	e2.cfg.Agents.Projects[codeKey] = pa
	write(t, filepath.Join(e2.folder, "info.json"), strings.Replace(infoJSON, `"my-mod"`, `"my&calc"`, 1))
	git(t, e2.folder, "commit", "-q", "-am", "rename")
	git(t, e2.folder, "push", "-q", "origin", "main")
	p, _ = e2.engine(false).Plan(t.Context(), e2.codeID, Request{})
	if p.OK || !slices.ContainsFunc(p.Refusals, func(r Refusal) bool {
		return r.Code == CodeNoProfile && strings.Contains(r.Message, "build.command") && strings.Contains(r.Message, "my&calc")
	}) {
		t.Fatalf("refusals %+v", p.Refusals)
	}
}

func TestReleasePushesUnpushedFolderCommits(t *testing.T) {
	e := newEnv(t)
	write(t, filepath.Join(e.folder, "control.lua"), "-- v1 fixed twice\n")
	git(t, e.folder, "commit", "-q", "-am", "Fix the second crash")
	local := git(t, e.folder, "rev-parse", "HEAD")
	en := e.engine(false)

	// Autopilot never pushes the owner's unpushed work.
	p, err := en.Plan(t.Context(), e.codeID, Request{Origin: store.RunOriginAuto})
	if err != nil || !slices.ContainsFunc(p.Refusals, func(r Refusal) bool { return r.Code == CodeHeadNotRemote }) {
		t.Fatalf("auto plan %v %+v", err, p.Refusals)
	}

	p, err = en.Plan(t.Context(), e.codeID, Request{})
	if err != nil || !p.OK || !slices.Equal(p.Unpushed, []string{local}) || p.RemoteHead != e.head {
		t.Fatalf("plan %v %+v unpushed %v", err, p.Refusals, p.Unpushed)
	}
	r := e.run(e.release(en, Request{}).ID)
	if r.State != store.RunDone {
		t.Fatalf("run %+v", r)
	}
	m, _ := manifestOf(r)
	if git(t, e.bare, "rev-parse", "main") != m.BumpSHA || git(t, e.bare, "rev-parse", "main^") != local {
		t.Fatal("remote main is not the bump on the folder's unpushed commit")
	}
	e.assertSentOnce(t)

	// Diverged (the remote has a commit the folder lacks) → refused.
	e2 := newEnv(t)
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", e2.bare, other)
	git(t, other, "-c", "user.name=x", "-c", "user.email=x@x", "commit", "-q", "--allow-empty", "-m", "remote work")
	git(t, other, "push", "-q", "origin", "main")
	git(t, e2.folder, "commit", "-q", "--allow-empty", "-m", "local work")
	p, _ = e2.engine(false).Plan(t.Context(), e2.codeID, Request{})
	if !slices.ContainsFunc(p.Refusals, func(r Refusal) bool {
		return r.Code == CodeHeadNotRemote && strings.Contains(r.Message, "diverged")
	}) {
		t.Fatalf("diverged refusals %+v", p.Refusals)
	}
}
