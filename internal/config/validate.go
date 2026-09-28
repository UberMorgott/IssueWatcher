package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
)

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Validate checks every field; the first problem is returned as *ValidationError.
func (s Settings) Validate() error {
	if !slices.Contains([]string{"ru", "en"}, s.General.Language) {
		return notOneOf("general.language", "ru", "en")
	}
	if err := s.Appearance.validate(); err != nil {
		return err
	}
	if err := s.Notifications.validate(); err != nil {
		return err
	}
	if err := s.Sync.validate(); err != nil {
		return err
	}
	if err := s.Updates.validate(); err != nil {
		return err
	}
	if err := s.Agents.validate(); err != nil {
		return err
	}
	return s.Projects.validate()
}

func (u Updates) validate() error {
	if !slices.Contains([]string{"stable", "preview"}, u.Channel) {
		return notOneOf("updates.channel", "stable", "preview")
	}
	if u.IntervalHours < 1 || u.IntervalHours > 168 {
		return outOfRange("updates.intervalHours", 1, 168)
	}
	return nil
}

func (n Notifications) validate() error {
	for field, v := range map[string]string{"notifications.quiet.from": n.Quiet.From, "notifications.quiet.to": n.Quiet.To} {
		if !hhmm.MatchString(v) {
			return invalid(field, "time", nil, "must be HH:MM")
		}
	}
	if n.Quiet.Enabled && n.Quiet.From == n.Quiet.To {
		return invalid("notifications.quiet.to", "differ", nil, "must differ from the start")
	}
	if n.AutoHideSeconds < 3 || n.AutoHideSeconds > 120 {
		return outOfRange("notifications.autoHideSeconds", 3, 120)
	}
	if len(n.MutedProjects) > 1000 {
		return invalid("notifications.mutedProjects", "tooMany", map[string]any{"max": 1000}, "at most 1000 entries")
	}
	return nil
}

func (s Sync) validate() error {
	if !slices.Contains([]string{SyncBalanced, SyncFast, SyncCustom}, s.Mode) {
		return notOneOf("sync.mode", SyncBalanced, SyncFast, SyncCustom)
	}
	if s.ActiveDays < 1 || s.ActiveDays > 365 {
		return outOfRange("sync.activeDays", 1, 365)
	}
	for id, p := range s.Providers {
		f := func(name string) string { return "sync.providers." + id + "." + name }
		switch {
		case p.ActiveMinutes < 1 || p.ActiveMinutes > 120:
			return outOfRange(f("activeMinutes"), 1, 120)
		case p.IdleMinutes < p.ActiveMinutes || p.IdleMinutes > 720:
			return outOfRange(f("idleMinutes"), p.ActiveMinutes, 720)
		case p.ReconcileMinutes < 15 || p.ReconcileMinutes > 1440:
			return outOfRange(f("reconcileMinutes"), 15, 1440)
		case p.HourlyBudget < 100 || p.HourlyBudget > 4500:
			return outOfRange(f("hourlyBudget"), 100, 4500)
		case p.Concurrency < 1 || p.Concurrency > 8:
			return outOfRange(f("concurrency"), 1, 8)
		}
	}
	return nil
}

func (p Projects) validate() error {
	if len(p.Roots) > 32 {
		return invalid("projects.roots", "tooMany", map[string]any{"max": 32}, "at most 32 folders")
	}
	for i, r := range p.Roots {
		if !filepath.IsAbs(r) {
			return invalid(fmt.Sprintf("projects.roots.%d", i), "absPath", nil, "must be an absolute path")
		}
	}
	if len(p.Exclude) > 100 {
		return invalid("projects.exclude", "tooMany", map[string]any{"max": 100}, "at most 100 names")
	}
	if p.ScanDepth < 1 || p.ScanDepth > 6 {
		return outOfRange("projects.scanDepth", 1, 6)
	}
	return nil
}
