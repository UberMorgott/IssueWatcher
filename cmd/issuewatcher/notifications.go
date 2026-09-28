package main

import (
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/notify"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// notifyPrefs maps Settings → Notifications to the popup filter.
func notifyPrefs(n config.Notifications) notify.Prefs {
	p := notify.Prefs{
		Enabled: n.Enabled,
		Kinds: map[store.EventKind]bool{
			store.EventNewIssue: n.NewIssue, store.EventNewComment: n.NewComment, store.EventClosed: n.Closed,
		},
		MutedRepos:        map[string]bool{},
		AutoHide:          time.Duration(n.AutoHideSeconds) * time.Second,
		RespectWindowsDnd: n.RespectDnd,
	}
	for _, r := range n.MutedProjects {
		p.MutedRepos[r] = true
	}
	if n.Quiet.Enabled {
		p.QuietFrom, p.QuietTo = minutes(n.Quiet.From), minutes(n.Quiet.To)
	}
	return p
}

// minutes parses HH:MM (validated by config) into minutes since midnight.
func minutes(hhmm string) int {
	h, m, _ := strings.Cut(hhmm, ":")
	hi, _ := strconv.Atoi(h)
	mi, _ := strconv.Atoi(m)
	return hi*60 + mi
}

// groupRepeats keeps one event per item (the last one of the sync), so a burst
// on one issue shows one card.
func groupRepeats(events []store.Event) []store.Event {
	last := map[int64]int{}
	for i, e := range events {
		last[e.ItemID] = i
	}
	out := make([]store.Event, 0, len(last))
	for i, e := range events {
		if last[e.ItemID] == i {
			out = append(out, e)
		}
	}
	return out
}

// liveFilter is what in-app toasts honour: event kinds and muted projects
// (quiet hours and the popup switch concern desktop popups only).
func liveFilter(n config.Notifications) notify.Prefs {
	p := notifyPrefs(n)
	p.Enabled, p.QuietFrom, p.QuietTo = true, 0, 0
	return p
}

// popupTheme follows the dashboard's theme mode (system → dark).
func popupTheme(a config.Appearance) notify.Theme {
	if a.Mode == "light" {
		return notify.LightTheme()
	}
	return notify.DarkTheme()
}
