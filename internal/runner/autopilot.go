package runner

import (
	"context"
	"slices"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/folders"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Autopilot fix jobs (docs/AUTOPILOT.md → Fix run): the fix step of a fix run
// is the existing direct fix job with rule origin (same caps), rule id
// store.RuleAutopilot and the "Refs #N" prompt variant.

// Skip reasons of AutopilotFix besides the store's and Automate's.
const (
	ReasonNoFixRule = "no_fix_rule" // no enabled fix rule of the project matches the item
	ReasonNotDirect = "not_direct"  // the code project is not in direct mode (autopilot pushes the folder's commits)
)

// AutopilotFixes reports whether autopilot fixes project key's items (its
// autopilot is on with autoFix): automation fix rules then leave them to it.
func AutopilotFixes(cfg config.Agents, key string) bool {
	ap := cfg.AutopilotFor(key)
	return ap.Enabled && ap.AutoFix
}

// AutopilotFix queues the fix job of an autopilot fix run for item itemID
// after event (new_issue / new_comment / new_item): an enabled fix rule of the
// item's project must match it (autopilot implies allowAutoFix), the code
// project must be in direct mode with its folder. The decision is logged in
// the automation log like a rule's; job is nil when it was skipped
// (entry.Reason; ReasonNoFixRule is not logged).
//
// triaged: the auto-triage verdict (a bug) decided the fix, so no rule is
// needed; a matching rule still sets the agent profile and its own cap.
func (r *Runner) AutopilotFix(ctx context.Context, itemID int64, event string, triaged bool) (store.AutomationEntry, *store.Job, error) {
	cfg := r.opts.Settings().Agents
	in, err := r.opts.Store.JobInput(ctx, itemID)
	if err != nil {
		return store.AutomationEntry{}, nil, err
	}
	facts, err := r.opts.Store.AutomationItemFacts(ctx, itemID)
	if err != nil {
		return store.AutomationEntry{}, nil, err
	}
	rule, ok, _, err := r.autopilotRule(ctx, itemID, event)
	if err != nil {
		return store.AutomationEntry{}, nil, err
	}
	if !ok && !triaged {
		return store.AutomationEntry{ItemID: itemID, Event: event, Flow: config.FlowFix, Decision: store.DecisionSkipped, Reason: ReasonNoFixRule}, nil, nil
	}
	pol := cfg.AutomationFor(in.CodeKey)
	profile := rule.ProfileID
	if profile == "" {
		profile = cfg.Roles.Coder
	}
	req := store.AutomationRequest{
		At: r.opts.Now(), ItemID: itemID, Event: event, RuleID: store.RuleAutopilot, Flow: config.FlowFix, ProfileID: profile,
		TotalCap: pol.TotalPerDay, DayCap: pol.ProjectPerDay, RuleCap: rule.MaxPerDay, MaxAttempts: pol.MaxAttempts,
	}
	switch {
	case !hasProfile(cfg, profile):
		req.Skip = ReasonNoProfile
	case !folders.Check(facts.LocalPath, facts.ProjectURL).Exists():
		req.Skip = ReasonNoFolder
	case cfg.ModeFor(in.CodeKey) != config.ModeDirect:
		req.Skip = ReasonNotDirect
	}
	e, j, err := r.opts.Store.Automate(ctx, req)
	if err != nil {
		return e, nil, err
	}
	r.opts.Log.Info("autopilot fix", "item", itemID, "rule", rule.ID, "decision", e.Decision, "reason", e.Reason)
	if j != nil {
		r.opts.OnJob(*j)
		r.kick()
	}
	return e, j, nil
}

// AutopilotMatch reports whether an enabled fix rule of the item's project
// covers item itemID for event (match: without auto-triage the inbox starts a
// fix run only then), and whether the project has enabled fix rules for the
// event and item kind at all (scoped: with auto-triage they filter which bugs
// are fixed; a project without such rules leaves it to the verdict).
func (r *Runner) AutopilotMatch(ctx context.Context, itemID int64, event string) (match, scoped bool, err error) {
	_, match, scoped, err = r.autopilotRule(ctx, itemID, event)
	return match, scoped, err
}

func (r *Runner) autopilotRule(ctx context.Context, itemID int64, event string) (config.Rule, bool, bool, error) {
	in, err := r.opts.Store.JobInput(ctx, itemID)
	if err != nil {
		return config.Rule{}, false, false, err
	}
	facts, err := r.opts.Store.AutomationItemFacts(ctx, itemID)
	if err != nil {
		return config.Rule{}, false, false, err
	}
	code, err := r.opts.Store.ItemCodeProject(ctx, itemID)
	if err != nil {
		return config.Rule{}, false, false, err
	}
	ev := store.Event{Kind: store.EventKind(event), ItemKind: code.Kind, ItemID: itemID, Project: in.ProjectKey}
	if in.Mod {
		ev.CodeProject = code.CodeKey
	}
	rules := r.opts.Settings().Agents.Automation.Rules
	scoped := slices.ContainsFunc(rules, func(ru config.Rule) bool { return ru.Flow == config.FlowFix && ruleFor(ru, ev) })
	ru, ok := fixRule(rules, ev, facts.Labels)
	return ru, ok, scoped, nil
}

func hasProfile(cfg config.Agents, id string) bool {
	_, ok := cfg.Profile(id)
	return ok
}

// fixRule is the first enabled fix rule covering the event whose labelsAny is
// empty or shares a label with the item.
func fixRule(rules []config.Rule, ev store.Event, labels []string) (config.Rule, bool) {
	for _, ru := range rules {
		if ru.Flow != config.FlowFix || !ruleFor(ru, ev) {
			continue
		}
		if len(ru.LabelsAny) == 0 || slices.ContainsFunc(ru.LabelsAny, func(want string) bool {
			return slices.ContainsFunc(labels, func(have string) bool { return strings.EqualFold(want, have) })
		}) {
			return ru, true
		}
	}
	return config.Rule{}, false
}

// HasGate reports whether project (platform:external_id) has a verify gate in
// localPath: Aegis enabled there, or a verify command (see Gate).
func (r *Runner) HasGate(project, localPath string) bool {
	argv, _ := r.verifyCommand(localPath, localPath, r.opts.Settings().Agents.Projects[project])
	return argv != nil
}
