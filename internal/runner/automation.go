package runner

import (
	"context"
	"slices"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/folders"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Skip reasons decided before the store transaction (the store adds exists,
// max_attempts, total_cap, day_cap, rule_cap).
const (
	ReasonNoProfile   = "no_profile"   // the rule's profile (or the flow's role) does not exist
	ReasonAutoFixOff  = "auto_fix_off" // fix rule where allowAutoFix is off
	ReasonNoFolder    = "no_folder"    // fix rule without a working mapped folder
	ReasonUnavailable = "unavailable"  // label rule without a label-capable provider
)

// Automate applies the automation rules to sync events (docs/ARCHITECTURE.md →
// Automation). Events already exclude the first-sync baseline and the user's
// own issues/comments. Per event: the project's effective settings must have
// automation on; the first enabled rule matching project + event + any label
// decides; every decision is logged. Over a cap = skipped, never deferred.
// It returns the logged decisions.
func (r *Runner) Automate(ctx context.Context, events []store.Event) []store.AutomationEntry {
	r.autoMu.Lock() // one batch at a time (the store transaction also serialises)
	defer r.autoMu.Unlock()
	cfg := r.opts.Settings().Agents
	var out []store.AutomationEntry
	type itemFlow struct {
		item int64
		flow string
	}
	seen := map[itemFlow]bool{} // one decision per item + flow and batch
	queued := false
	for _, ev := range events {
		pol := cfg.AutomationFor(ev.Repo)
		if !pol.Enabled || (ev.Kind != store.EventNewIssue && ev.Kind != store.EventNewComment) {
			continue
		}
		hasRule := slices.ContainsFunc(cfg.Automation.Rules, func(ru config.Rule) bool {
			return ru.Enabled && strings.EqualFold(ru.Project, ev.Repo) && ru.Event == string(ev.Kind)
		})
		if !hasRule {
			continue
		}
		item, err := r.opts.Store.AutomationItemFacts(ctx, ev.ItemID)
		if err != nil {
			r.opts.Log.Error("automation: item", "item", ev.ItemID, "err", err)
			continue
		}
		rule, ok := matchRule(cfg.Automation.Rules, ev, item.Labels)
		if !ok {
			continue
		}
		key := itemFlow{ev.ItemID, rule.Flow}
		if seen[key] {
			continue
		}
		seen[key] = true
		profile := rule.ProfileID
		if profile == "" {
			profile = cfg.Roles.Coder
			if rule.Flow != config.FlowFix {
				profile = cfg.Roles.Responder
			}
		}
		req := store.AutomationRequest{
			At: r.opts.Now(), ItemID: ev.ItemID, Event: string(ev.Kind), RuleID: rule.ID, Flow: rule.Flow, ProfileID: profile,
			TotalCap: pol.TotalPerDay, DayCap: pol.ProjectPerDay, RuleCap: rule.MaxPerDay, MaxAttempts: pol.MaxAttempts,
		}
		if _, ok := cfg.Profile(profile); !ok {
			req.Skip = ReasonNoProfile
		} else if rule.Flow == config.FlowLabel && r.opts.Labels == nil {
			req.Skip = ReasonUnavailable
		} else if rule.Flow == config.FlowFix && !pol.AllowAutoFix {
			req.Skip = ReasonAutoFixOff
		} else if rule.Flow == config.FlowFix && folders.Check(item.LocalPath, item.ProjectURL) != folders.StatusOK {
			req.Skip = ReasonNoFolder
		}
		e, j, err := r.opts.Store.Automate(ctx, req)
		if err != nil {
			r.opts.Log.Error("automation: decide", "item", ev.ItemID, "rule", rule.ID, "err", err)
			continue
		}
		r.opts.Log.Info("automation", "item", ev.ItemID, "rule", rule.ID, "flow", rule.Flow, "decision", e.Decision, "reason", e.Reason)
		out = append(out, e)
		if j != nil {
			queued = true
			r.opts.OnJob(*j)
		}
	}
	if queued {
		r.kick()
	}
	return out
}

// matchRule returns the first enabled rule for the event's project and kind
// whose labelsAny is empty or shares a label with the item (case-insensitive).
func matchRule(rules []config.Rule, ev store.Event, labels []string) (config.Rule, bool) {
	for _, ru := range rules {
		if !ru.Enabled || !strings.EqualFold(ru.Project, ev.Repo) || ru.Event != string(ev.Kind) {
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
