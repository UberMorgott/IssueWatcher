package main

import (
	"image/color"
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
			store.EventNewItem: n.NewIssue, // new mod-page threads / bug reports follow the «new issue» switch
		},
		MutedProjects:     map[string]bool{},
		AutoHide:          time.Duration(n.AutoHideSeconds) * time.Second,
		RespectWindowsDnd: n.RespectDnd,
	}
	for _, r := range n.MutedProjects {
		p.MutedProjects[r] = true
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

// popupTheme gives the popups the dashboard's colours: the effective palette
// of the current mode (system → dark), with the same derivations the UI uses
// (frontend/src/lib/appearance.ts tokens).
func popupTheme(a config.Appearance) notify.Theme {
	mode := "dark"
	if a.Mode == "light" {
		mode = "light"
	}
	c := a.Effective(mode)
	base := notify.DarkTheme()
	if mode == "light" {
		base = notify.LightTheme()
	}
	bg, text := hexColor(c.Surface), hexColor(c.Text)
	return notify.Theme{
		Bg:      mix(bg, text, 0.06),
		Surface: mix(bg, text, 0.14),
		Border:  mix(bg, text, 0.22),
		Text:    text,
		Muted:   mix(text, bg, 0.38),
		Accent:  hexColor(c.Accent),
		Success: base.Success,
		Shadow:  base.Shadow,
	}
}

func hexColor(h string) color.NRGBA {
	v, _ := strconv.ParseUint(strings.TrimPrefix(h, "#"), 16, 32)
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff} //nolint:gosec // G115: masked 8-bit channels
}

// mix moves a towards b by w (0..1).
func mix(a, b color.NRGBA, w float64) color.NRGBA {
	f := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*w + 0.5) }
	return color.NRGBA{R: f(a.R, b.R), G: f(a.G, b.G), B: f(a.B, b.B), A: 0xff}
}
