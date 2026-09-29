package config

import (
	"fmt"
	"regexp"
	"slices"
)

// Automation: rules that queue agent jobs on sync events (docs/ARCHITECTURE.md
// → Automation). Every behaviour here is the global default; a project can
// override it in ProjectAgent.Automation.
type Automation struct {
	Enabled bool `json:"enabled"`
	// MaxPerDay caps rule-made jobs in any 24 h window, 1–500.
	MaxPerDay int `json:"maxPerDay"`
	// MaxAttempts caps rule-made jobs per item and flow, 1–10.
	MaxAttempts int `json:"maxAttempts"`
	// AllowAutoFix lets rules queue fix jobs (direct mode edits the working folder unattended).
	AllowAutoFix bool `json:"allowAutoFix"`
	// AutoApplyLabels adds a rule-made label job's suggestions without a click (additive only).
	AutoApplyLabels bool   `json:"autoApplyLabels"`
	Rules           []Rule `json:"rules"` // ordered: the first matching enabled rule wins
}

// Rule events and flows.
const (
	EventNewIssue   = "new_issue"
	EventNewComment = "new_comment"
	FlowFix         = "fix"
	FlowReply       = "reply"
	FlowLabel       = "label"
)

// Rule matches a sync event and queues one job.
type Rule struct {
	ID        string   `json:"id"` // [a-z0-9-], unique
	Enabled   bool     `json:"enabled"`
	Project   string   `json:"project"`   // owner/repo
	Event     string   `json:"event"`     // new_issue | new_comment
	LabelsAny []string `json:"labelsAny"` // empty = any item
	Flow      string   `json:"flow"`      // fix | reply | label
	ProfileID string   `json:"profileId"` // "" = the flow's role
	// MaxPerDay caps this rule's jobs in 24 h, 0–500; 0 = only the global/project cap.
	MaxPerDay int `json:"maxPerDay"`
}

// ProjectAutomation overrides Automation for one project; nil = inherit.
type ProjectAutomation struct {
	Enabled         *bool `json:"enabled,omitempty"`
	MaxPerDay       *int  `json:"maxPerDay,omitempty"`
	MaxAttempts     *int  `json:"maxAttempts,omitempty"`
	AllowAutoFix    *bool `json:"allowAutoFix,omitempty"`
	AutoApplyLabels *bool `json:"autoApplyLabels,omitempty"`
}

// AutomationPolicy is the effective automation setup of one project.
type AutomationPolicy struct {
	Enabled         bool
	MaxPerDay       int
	MaxAttempts     int
	AllowAutoFix    bool
	AutoApplyLabels bool
}

func defaultAutomation() Automation {
	return Automation{MaxPerDay: 10, MaxAttempts: 2, Rules: []Rule{}}
}

// AutomationFor resolves project's (owner/repo) overrides over the global defaults.
func (a Agents) AutomationFor(project string) AutomationPolicy {
	g := a.Automation
	p := AutomationPolicy{Enabled: g.Enabled, MaxPerDay: g.MaxPerDay, MaxAttempts: g.MaxAttempts,
		AllowAutoFix: g.AllowAutoFix, AutoApplyLabels: g.AutoApplyLabels}
	o := a.Projects[project].Automation
	for _, f := range []struct {
		dst *bool
		src *bool
	}{{&p.Enabled, o.Enabled}, {&p.AllowAutoFix, o.AllowAutoFix}, {&p.AutoApplyLabels, o.AutoApplyLabels}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	if o.MaxPerDay != nil {
		p.MaxPerDay = *o.MaxPerDay
	}
	if o.MaxAttempts != nil {
		p.MaxAttempts = *o.MaxAttempts
	}
	return p
}

func (au *Automation) normalize() {
	if au.Rules == nil {
		au.Rules = []Rule{}
	}
	for i := range au.Rules {
		if au.Rules[i].LabelsAny == nil {
			au.Rules[i].LabelsAny = []string{}
		}
	}
}

var projectName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

const (
	maxRules      = 100
	maxRuleLabels = 20
	maxPerDayCap  = 500
	maxAttemptCap = 10
)

// validate checks the block; profiles are the configured profile ids.
func (au Automation) validate(profiles map[string]bool) error {
	const base = "agents.automation."
	switch {
	case au.MaxPerDay < 1 || au.MaxPerDay > maxPerDayCap:
		return outOfRange(base+"maxPerDay", 1, maxPerDayCap)
	case au.MaxAttempts < 1 || au.MaxAttempts > maxAttemptCap:
		return outOfRange(base+"maxAttempts", 1, maxAttemptCap)
	case len(au.Rules) > maxRules:
		return invalid(base+"rules", "tooMany", map[string]any{"max": maxRules}, "at most %d rules", maxRules)
	}
	seen := map[string]bool{}
	for i, r := range au.Rules {
		f := func(name string) string { return fmt.Sprintf("%srules.%d.%s", base, i, name) }
		switch {
		case !profileID.MatchString(r.ID):
			return invalid(f("id"), "id", nil, "must be 1–32 of a-z, 0-9, -")
		case seen[r.ID]:
			return invalid(f("id"), "duplicate", nil, "duplicate id %q", r.ID)
		case !projectName.MatchString(r.Project):
			return invalid(f("project"), "project", nil, "must be owner/repo")
		case !slices.Contains([]string{EventNewIssue, EventNewComment}, r.Event):
			return notOneOf(f("event"), EventNewIssue, EventNewComment)
		case !slices.Contains([]string{FlowFix, FlowReply, FlowLabel}, r.Flow):
			return notOneOf(f("flow"), FlowFix, FlowReply, FlowLabel)
		case r.ProfileID != "" && !profiles[r.ProfileID]:
			return invalid(f("profileId"), "profile", map[string]any{"id": r.ProfileID}, "no profile %q", r.ProfileID)
		case r.MaxPerDay < 0 || r.MaxPerDay > maxPerDayCap:
			return outOfRange(f("maxPerDay"), 0, maxPerDayCap)
		case len(r.LabelsAny) > maxRuleLabels:
			return invalid(f("labelsAny"), "tooMany", map[string]any{"max": maxRuleLabels}, "at most %d labels", maxRuleLabels)
		}
		for _, l := range r.LabelsAny {
			if l == "" || len(l) > 100 {
				return invalid(f("labelsAny"), "required", nil, "labels must be 1–100 characters")
			}
		}
		seen[r.ID] = true
	}
	return nil
}

func (o ProjectAutomation) validate(project string) error {
	base := "agents.projects." + project + ".automation."
	if o.MaxPerDay != nil && (*o.MaxPerDay < 1 || *o.MaxPerDay > maxPerDayCap) {
		return outOfRange(base+"maxPerDay", 1, maxPerDayCap)
	}
	if o.MaxAttempts != nil && (*o.MaxAttempts < 1 || *o.MaxAttempts > maxAttemptCap) {
		return outOfRange(base+"maxAttempts", 1, maxAttemptCap)
	}
	return nil
}
