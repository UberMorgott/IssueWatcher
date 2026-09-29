package runner

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// labelRepo gives octo/demo labels on the fake GitHub and its issue #1 the
// label wontfix there and locally.
func (e *env) labelRepo() {
	e.t.Helper()
	e.gh.Mu.Lock()
	e.gh.Labels = map[string][]string{"octo/demo": {"bug", "documentation", "wontfix", "enhancement"}}
	e.gh.Issues = []*githubtest.Issue{{ID: "I_0_1", Repo: "octo/demo", Number: 1, Title: "Crash #1 on start", Open: true, Labels: []string{"wontfix"}}}
	e.gh.Mu.Unlock()
	if err := e.st.SetItemLabels(e.t.Context(), e.items[0], []string{"wontfix"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) ghState() (issue, repo []string, adds int) {
	e.gh.Mu.Lock()
	defer e.gh.Mu.Unlock()
	return slices.Clone(e.gh.Issues[0].Labels), slices.Clone(e.gh.Labels["octo/demo"]), e.gh.LabelAdds
}

func TestLabelFlowSuggestAndApply(t *testing.T) {
	mode(t, "ok")
	t.Setenv("FAKECLI_LABELS", "BUG,Documentation,made-up,bug")
	e := setup(t, 1, nil)
	e.labelRepo()
	j := e.wait(e.enqueue("label", e.items[0])[0].ID, store.JobNeedsReview)
	res := result(t, j)
	if !slices.Equal(res.Labels, []string{"bug", "documentation"}) || !slices.Equal(res.DroppedLabels, []string{"made-up"}) ||
		res.Agent == nil || res.Agent.CLI != "codex" || j.ProfileID != "codex" {
		t.Fatalf("suggestions: %+v %+v", res, res.Agent)
	}
	b, _ := os.ReadFile(e.record)
	if !strings.Contains(string(b), `- documentation`) || !strings.Contains(string(b), `"read-only"`) {
		t.Fatalf("label list not in the prompt / not read-only: %s", b)
	}
	if exists(e.local + "/fixed.txt") {
		t.Fatal("label flow changed files")
	}

	// Unknown name: rejected, nothing sent, the job stays under review.
	if _, err := e.r.ApplyLabels(t.Context(), j.ID, []string{"bug", "made-up"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown label: %v", err)
	}
	if _, _, adds := e.ghState(); adds != 0 {
		t.Fatal("unknown label reached GitHub")
	}
	if _, err := e.r.ApplyLabels(t.Context(), j.ID, []string{" "}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("empty labels: %v", err)
	}
	j, err := e.r.ApplyLabels(t.Context(), j.ID, []string{"Bug", "wontfix"})
	if err != nil || j.State != store.JobDone {
		t.Fatalf("apply: %+v %v", j, err)
	}
	if res := result(t, j); !slices.Equal(res.AppliedLabels, []string{"bug"}) || res.PublishError != "" {
		t.Fatalf("applied: %+v", res)
	}
	issue, repo, adds := e.ghState()
	if !slices.Equal(issue, []string{"wontfix", "bug"}) || len(repo) != 4 || adds != 1 {
		t.Fatalf("GitHub after apply: issue %v repo %v adds %d", issue, repo, adds)
	}
	if in, _ := e.st.JobInput(t.Context(), e.items[0]); !slices.Equal(in.Labels, []string{"wontfix", "bug"}) {
		t.Fatalf("local labels: %v", in.Labels)
	}
	if _, err := e.r.ApplyLabels(t.Context(), j.ID, []string{"bug"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("apply twice: %v", err)
	}
}

// A rule job adds its picks itself only where autoApplyLabels is on (project
// override over the global default); a manual job never does.
func TestLabelAutoApply(t *testing.T) {
	mode(t, "ok")
	t.Setenv("FAKECLI_LABELS", "documentation")
	off := false
	e := setup(t, 1, func(s *config.Settings) {
		s.Agents.Automation.AutoApplyLabels = true
		s.Agents.Projects["octo/demo"] = config.ProjectAgent{Mode: config.ModeDirect, Automation: config.ProjectAutomation{AutoApplyLabels: &off}}
	})
	e.labelRepo()
	rule := func() store.Job {
		t.Helper()
		j, err := e.st.CreateJob(t.Context(), e.items[0], "label", "codex", store.OriginRule, "r1")
		if err != nil {
			t.Fatal(err)
		}
		e.r.Refresh()
		return e.wait(j.ID, store.JobNeedsReview, store.JobDone, store.JobFailed)
	}
	if j := rule(); j.State != store.JobNeedsReview {
		t.Fatalf("project override off: %+v", j)
	}
	jobs, _ := e.st.JobsInState(t.Context(), store.JobNeedsReview)
	for _, j := range jobs {
		if _, err := e.r.Dismiss(t.Context(), j.ID); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	e.cfg.Agents.Projects["octo/demo"] = config.ProjectAgent{Mode: config.ModeDirect}
	e.mu.Unlock()
	j := rule()
	if res := result(t, j); j.State != store.JobDone || !slices.Equal(res.AppliedLabels, []string{"documentation"}) {
		t.Fatalf("auto-apply: %+v %+v", j, res)
	}
	issue, _, adds := e.ghState()
	if !slices.Equal(issue, []string{"wontfix", "documentation"}) || adds != 1 {
		t.Fatalf("GitHub after auto-apply: %v %d", issue, adds)
	}
	// Picks already on the issue: done, nothing sent again.
	j = rule()
	if res := result(t, j); j.State != store.JobDone || len(res.AppliedLabels) != 0 {
		t.Fatalf("re-apply: %+v %+v", j, res)
	}
	if _, _, adds := e.ghState(); adds != 1 {
		t.Fatalf("re-apply sent labels again: %d", adds)
	}
	// A manual job with auto-apply on still waits for the click.
	if j := e.wait(e.enqueue("label", e.items[0])[0].ID, store.JobNeedsReview, store.JobDone); j.State != store.JobNeedsReview {
		t.Fatalf("manual job applied itself: %+v", j)
	}
}

// A rule job whose agent finds no fitting label ends done (nothing to apply
// or review) instead of blocking the issue's next label job; a manual one
// still waits for the user, who may pick labels by hand.
func TestLabelRuleNoPicksDone(t *testing.T) {
	mode(t, "ok")
	t.Setenv("FAKECLI_LABELS", "")
	e := setup(t, 1, nil)
	e.labelRepo()
	j, err := e.st.CreateJob(t.Context(), e.items[0], "label", "codex", store.OriginRule, "r1")
	if err != nil {
		t.Fatal(err)
	}
	e.r.Refresh()
	if j = e.wait(j.ID, store.JobNeedsReview, store.JobDone, store.JobFailed); j.State != store.JobDone || len(result(t, j).Labels) != 0 {
		t.Fatalf("rule job without picks: %+v", j)
	}
	if _, _, adds := e.ghState(); adds != 0 {
		t.Fatalf("labels sent: %d", adds)
	}
	if j := e.wait(e.enqueue("label", e.items[0])[0].ID, store.JobNeedsReview, store.JobDone); j.State != store.JobNeedsReview {
		t.Fatalf("manual job without picks: %+v", j)
	}
}

func TestMatchLabels(t *testing.T) {
	repo := []provider.Label{{Name: "Bug"}, {Name: "good first issue"}}
	known, unknown := matchLabels(repo, []string{"bug", " Good First Issue ", "BUG", "", "nope"})
	if !slices.Equal(known, []string{"Bug", "good first issue"}) || !slices.Equal(unknown, []string{"nope"}) {
		t.Fatalf("known %v unknown %v", known, unknown)
	}
}
