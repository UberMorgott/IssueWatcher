package notify

import (
	"time"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// DefaultAutoHide is how long a card stays when the settings name no value.
const DefaultAutoHide = 8 * time.Second

// Prefs are the app's own notification settings. Windows Do Not Disturb does
// not apply to these popups unless RespectWindowsDnd is set.
type Prefs struct {
	Enabled           bool
	Kinds             map[store.EventKind]bool // nil = every kind
	MutedProjects     map[string]bool          // project key platform:external_id → no popups (names repeat across platforms)
	QuietFrom         int                      // quiet hours, minutes since local midnight;
	QuietTo           int                      // QuietFrom == QuietTo = off; may wrap past midnight
	AutoHide          time.Duration
	RespectWindowsDnd bool // skip popups while WindowsBusy (full-screen app, presentation)
}

// DefaultPrefs: every kind, no quiet hours, DefaultAutoHide.
func DefaultPrefs() Prefs { return Prefs{Enabled: true, AutoHide: DefaultAutoHide} }

// Filter keeps the events that should pop up at now.
func (p Prefs) Filter(events []store.Event, now time.Time) []store.Event {
	if !p.Enabled || p.Quiet(now) {
		return nil
	}
	var out []store.Event
	for _, e := range events {
		if p.Kinds != nil && !p.Kinds[e.Kind] {
			continue
		}
		if p.MutedProjects[projectKey(e)] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// projectKey is the event's settings key; an event without one is GitHub's.
func projectKey(e store.Event) string {
	if e.Project != "" {
		return e.Project
	}
	return "github:" + e.Repo
}

// Pick returns the cards to pop up for events at now. busy reports whether
// Windows asks apps to hold notifications; it is asked only with
// RespectWindowsDnd.
func (p Prefs) Pick(events []store.Event, now time.Time, busy func() bool) []Card {
	if p.RespectWindowsDnd && busy() {
		return nil
	}
	return Cards(p.Filter(events, now), now)
}

// Quiet reports whether now falls into the quiet hours [QuietFrom, QuietTo).
func (p Prefs) Quiet(now time.Time) bool {
	if p.QuietFrom == p.QuietTo {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if p.QuietFrom < p.QuietTo {
		return m >= p.QuietFrom && m < p.QuietTo
	}
	return m >= p.QuietFrom || m < p.QuietTo // 22:00–07:00
}
