package runner

import (
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// autoSetup: n issues per project, nothing ever launches (maxParallel 0) so
// rule jobs stay queued; edit sets rules and policies.
func autoSetup(t *testing.T, n int, edit func(*config.Settings)) *env {
	t.Helper()
	return setup(t, n, func(s *config.Settings) {
		s.Agents.MaxParallel = 0
		s.Agents.Automation.Enabled = true
		if edit != nil {
			edit(s)
		}
	})
}

func ev(kind store.EventKind, repo string, item int64) store.Event {
	return store.Event{Kind: kind, Project: "github:" + repo, Repo: repo, ItemID: item}
}

func (e *env) set(f func(*config.Settings)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f(&e.cfg)
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.clock = e.clock.Add(d); e.mu.Unlock() }

type decision struct{ rule, decision, reason string }

func decisions(entries []store.AutomationEntry) []decision {
	out := []decision{}
	for _, x := range entries {
		out = append(out, decision{x.RuleID, x.Decision, x.Reason})
	}
	return out
}

func wantDecisions(t *testing.T, got []store.AutomationEntry, want ...decision) {
	t.Helper()
	g := decisions(got)
	if len(g) != len(want) {
		t.Fatalf("decisions %+v, want %+v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("decisions %+v, want %+v", g, want)
		}
	}
}

func rule(id, project, event, flow string, labels ...string) config.Rule {
	if labels == nil {
		labels = []string{}
	}
	return config.Rule{ID: id, Enabled: true, Project: "github:" + project, Event: event, Flow: flow, LabelsAny: labels}
}

func TestAutomateMatching(t *testing.T) {
	e := autoSetup(t, 3, func(s *config.Settings) {
		s.Agents.Automation.Enabled = false
		off := rule("comments", "octo/demo", config.EventNewComment, config.FlowReply)
		off.Enabled = false
		s.Agents.Automation.Rules = []config.Rule{
			rule("bugs", "octo/demo", config.EventNewIssue, config.FlowReply, "Bug"),
			rule("all", "octo/demo", config.EventNewIssue, config.FlowLabel),
			off,
		}
	})
	ctx := t.Context()
	if err := e.st.SetItemLabels(ctx, e.items[0], []string{"bug"}); err != nil {
		t.Fatal(err)
	}
	batch := []store.Event{
		ev(store.EventNewIssue, "octo/demo", e.items[0]),   // labelled → "bugs"
		ev(store.EventNewIssue, "octo/demo", e.items[1]),   // unlabelled → "all"
		ev(store.EventNewComment, "octo/demo", e.items[2]), // only a disabled rule
		ev(store.EventNewIssue, "octo/other", e.items[3]),  // no rule for the project
		ev(store.EventClosed, "octo/demo", e.items[2]),     // not a rule event
	}
	// Automation off: nothing at all.
	if got := e.r.Automate(ctx, batch); len(got) != 0 {
		t.Fatalf("automation off: %+v", got)
	}
	// A project override turns it on for octo/demo only.
	on := true
	e.set(func(s *config.Settings) {
		s.Agents.Projects["github:octo/demo"] = config.ProjectAgent{Mode: config.ModeDirect, Automation: config.ProjectAutomation{Enabled: &on}}
	})
	got := e.r.Automate(ctx, batch)
	wantDecisions(t, got, decision{"bugs", "queued", ""}, decision{"all", "queued", ""})
	for i, x := range got {
		j, err := e.st.Job(ctx, *x.JobID)
		if err != nil || j.Origin != store.OriginRule || j.RuleID != x.RuleID || j.State != store.JobQueued ||
			j.ItemID != e.items[i] || j.Flow != []string{"reply", "label"}[i] || j.ProfileID != "codex" {
			t.Fatalf("job %d: %+v %v", i, j, err)
		}
	}
	// Same events again: the unfinished jobs block new ones (logged).
	wantDecisions(t, e.r.Automate(ctx, batch[:2]), decision{"bugs", "skipped", "exists"}, decision{"all", "skipped", "exists"})
	// Duplicate item+flow in one batch → one decision.
	wantDecisions(t, e.r.Automate(ctx, []store.Event{batch[1], batch[1]}), decision{"all", "skipped", "exists"})

	log, err := e.st.AutomationLog(ctx, "", 2)
	if err != nil || len(log.Items) != 2 || !log.More || log.Items[0].Repo != "octo/demo" || log.Items[0].Number != 2 {
		t.Fatalf("log: %+v %v", log, err)
	}
	rest, err := e.st.AutomationLog(ctx, log.NextCursor, 50)
	if err != nil || len(rest.Items) != 3 || rest.More || rest.Items[2].Decision != "queued" || rest.Items[2].JobID == nil {
		t.Fatalf("log rest: %+v %v", rest, err)
	}
}

func TestAutomateCaps(t *testing.T) {
	e := autoSetup(t, 9, func(s *config.Settings) {
		s.Agents.Automation.MaxPerDay = 2
		s.Agents.Automation.Rules = []config.Rule{
			rule("all", "octo/demo", config.EventNewIssue, config.FlowReply),
			rule("other", "octo/other", config.EventNewIssue, config.FlowReply),
		}
	})
	ctx := t.Context()
	decide := func(repo string, item int64) decision {
		t.Helper()
		got := e.r.Automate(ctx, []store.Event{ev(store.EventNewIssue, repo, item)})
		if len(got) != 1 {
			t.Fatalf("item %d: %+v", item, got)
		}
		return decisions(got)[0]
	}
	auto := func(i int) decision { t.Helper(); return decide("octo/demo", e.items[i]) }
	other := func(i int) decision { t.Helper(); return decide("octo/other", e.items[9+i]) }
	queued, totalCap, dayCap, ruleCap := decision{"all", "queued", ""}, decision{"all", "skipped", "total_cap"},
		decision{"all", "skipped", "day_cap"}, decision{"all", "skipped", "rule_cap"}
	// Global maxPerDay 2 = total across all projects, rolling 24 h.
	for i, want := range []decision{queued, queued, totalCap} {
		if got := auto(i); got != want {
			t.Fatalf("item %d: %+v, want %+v", i, got, want)
		}
	}
	if got := other(0); got != (decision{"other", "skipped", "total_cap"}) {
		t.Fatalf("total spans projects: %+v", got)
	}
	e.advance(23 * time.Hour)
	if got := auto(2); got != totalCap {
		t.Fatalf("after 23 h: %+v", got)
	}
	e.advance(90 * time.Minute) // the first two left the window
	if got := auto(2); got != queued {
		t.Fatalf("after 24.5 h: %+v", got)
	}
	// Rule cap on top: 1 per day even though the total allows 2.
	e.advance(25 * time.Hour)
	e.set(func(s *config.Settings) { s.Agents.Automation.Rules[0].MaxPerDay = 1 })
	if a, b := auto(3), auto(4); a != queued || b != ruleCap {
		t.Fatalf("rule cap: %+v %+v", a, b)
	}
	// Project override = extra per-project cap under the total; other projects unaffected.
	e.advance(25 * time.Hour)
	one := 1
	e.set(func(s *config.Settings) {
		s.Agents.Automation.MaxPerDay = 5
		s.Agents.Automation.Rules[0].MaxPerDay = 0
		s.Agents.Projects["github:octo/demo"] = config.ProjectAgent{Mode: config.ModeDirect, Automation: config.ProjectAutomation{MaxPerDay: &one}}
	})
	if a, b, c := auto(4), auto(5), other(1); a != queued || b != dayCap || c != (decision{"other", "queued", ""}) {
		t.Fatalf("project cap: %+v %+v %+v", a, b, c)
	}
	// Two concurrent batches against the last free slot of a cap of 1 → one job.
	e.advance(25 * time.Hour)
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []store.AutomationEntry
	)
	for _, i := range []int{7, 8} {
		wg.Go(func() {
			got := e.r.Automate(ctx, []store.Event{ev(store.EventNewIssue, "octo/demo", e.items[i])})
			mu.Lock()
			all = append(all, got...)
			mu.Unlock()
		})
	}
	wg.Wait()
	n := 0
	for _, x := range all {
		if x.Decision == store.DecisionQueued {
			n++
		}
	}
	if len(all) != 2 || n != 1 {
		t.Fatalf("concurrent: %+v", decisions(all))
	}
}

func TestAutomateMaxAttempts(t *testing.T) {
	e := autoSetup(t, 1, func(s *config.Settings) {
		s.Agents.Automation.Rules = []config.Rule{rule("c", "octo/demo", config.EventNewComment, config.FlowReply)}
	})
	ctx := t.Context()
	batch := []store.Event{ev(store.EventNewComment, "octo/demo", e.items[0])}
	for attempt := 1; attempt <= 2; attempt++ {
		got := e.r.Automate(ctx, batch)
		if len(got) != 1 || got[0].Decision != store.DecisionQueued {
			t.Fatalf("attempt %d: %+v", attempt, decisions(got))
		}
		if _, err := e.r.Cancel(ctx, *got[0].JobID); err != nil {
			t.Fatal(err)
		}
	}
	// maxAttempts 2 (default): a third rule job for item+flow is refused; a manual job is not affected.
	wantDecisions(t, e.r.Automate(ctx, batch), decision{"c", "skipped", "max_attempts"})
	e.enqueue("reply", e.items[0])
}

func TestAutomateFixGateAndProfile(t *testing.T) {
	e := autoSetup(t, 2, func(s *config.Settings) {
		ghost := rule("ghost", "octo/demo", config.EventNewComment, config.FlowReply)
		ghost.ProfileID = "nobody"
		s.Agents.Automation.Rules = []config.Rule{
			rule("fix-demo", "octo/demo", config.EventNewIssue, config.FlowFix),
			rule("fix-other", "octo/other", config.EventNewIssue, config.FlowFix),
			ghost,
		}
	})
	ctx := t.Context()
	demo := []store.Event{ev(store.EventNewIssue, "octo/demo", e.items[0])}
	other := []store.Event{ev(store.EventNewIssue, "octo/other", e.items[2])}
	// allowAutoFix off (default): fix rules are logged as skipped.
	wantDecisions(t, e.r.Automate(ctx, demo), decision{"fix-demo", "skipped", "auto_fix_off"})
	// Project override on for octo/demo (mapped folder) → queued with the coder profile.
	on := true
	e.set(func(s *config.Settings) {
		s.Agents.Projects["github:octo/demo"] = config.ProjectAgent{Mode: config.ModeDirect, Automation: config.ProjectAutomation{AllowAutoFix: &on}}
	})
	got := e.r.Automate(ctx, demo)
	wantDecisions(t, got, decision{"fix-demo", "queued", ""})
	if j, _ := e.st.Job(ctx, *got[0].JobID); j.Flow != "fix" || j.ProfileID != "claude" {
		t.Fatalf("fix job: %+v", j)
	}
	// Still off for octo/other; on globally, it has no mapped folder.
	wantDecisions(t, e.r.Automate(ctx, other), decision{"fix-other", "skipped", "auto_fix_off"})
	e.set(func(s *config.Settings) { s.Agents.Automation.AllowAutoFix = true })
	wantDecisions(t, e.r.Automate(ctx, other), decision{"fix-other", "skipped", "no_folder"})
	// A rule whose profile is gone.
	wantDecisions(t, e.r.Automate(ctx, []store.Event{ev(store.EventNewComment, "octo/demo", e.items[1])}),
		decision{"ghost", "skipped", "no_profile"})
}

func TestAutomateKinds(t *testing.T) {
	modReplies := rule("mod-replies", "octo/demo", config.EventNewComment, config.FlowLabel)
	modReplies.Kinds = []string{"comment", "bug"}
	modThreads := rule("mod-threads", "octo/demo", config.EventNewItem, config.FlowReply)
	modThreads.Kinds = []string{"comment"}
	e := autoSetup(t, 3, func(s *config.Settings) {
		s.Agents.Automation.Rules = []config.Rule{
			rule("issue-comments", "octo/demo", config.EventNewComment, config.FlowReply), // no kinds = issues only
			modThreads, modReplies,
		}
	})
	kinded := func(kind store.EventKind, item int64, itemKind string) store.Event {
		x := ev(kind, "octo/demo", item)
		x.ItemKind = itemKind
		return x
	}
	got := e.r.Automate(t.Context(), []store.Event{
		kinded(store.EventNewComment, e.items[0], store.KindComment), // skips the issue rule → mod-replies
		kinded(store.EventNewItem, e.items[1], store.KindComment),    // mod-threads
		kinded(store.EventNewItem, e.items[2], store.KindBug),        // no rule for new bug reports
		kinded(store.EventNewComment, e.items[2], ""),                // legacy event = issue → issue-comments
	})
	wantDecisions(t, got, decision{"mod-replies", "queued", ""}, decision{"mod-threads", "queued", ""},
		decision{"issue-comments", "queued", ""})
}

// A code project's rules and automation policy cover its linked mod pages'
// items (the mod page's own override no longer decides).
func TestAutomateLinkedModPage(t *testing.T) {
	threads := rule("mod-threads", "octo/demo", config.EventNewItem, config.FlowReply)
	threads.Kinds = []string{"comment"}
	on, off := true, false
	e := autoSetup(t, 1, func(s *config.Settings) {
		s.Agents.Automation.Enabled = false
		s.Agents.Automation.Rules = []config.Rule{threads}
		s.Agents.Projects["github:octo/demo"] = config.ProjectAgent{Mode: config.ModeDirect, Automation: config.ProjectAutomation{Enabled: &on}}
		s.Agents.Projects["nexus:skyrim/7"] = config.ProjectAgent{Mode: config.ModeDirect, Automation: config.ProjectAutomation{Enabled: &off}}
	})
	x := store.Event{Kind: store.EventNewItem, ItemKind: store.KindComment, Project: "nexus:skyrim/7", Repo: "Demo Mod", ItemID: e.items[0]}
	if got := e.r.Automate(t.Context(), []store.Event{x}); len(got) != 0 {
		t.Fatalf("unlinked mod page: %+v", decisions(got))
	}
	x.CodeProject = "github:octo/demo"
	wantDecisions(t, e.r.Automate(t.Context(), []store.Event{x}), decision{"mod-threads", "queued", ""})
}
