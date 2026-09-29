package main

import (
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

func TestNotifyPrefsFromSettings(t *testing.T) {
	n := config.Defaults().Notifications
	n.NewComment = false
	n.MutedProjects = []string{"github:o/muted", "nexus:windrose/147"}
	n.Quiet = config.QuietHours{Enabled: true, From: "22:30", To: "07:00"}
	n.AutoHideSeconds = 12
	p := notifyPrefs(n)
	if p.QuietFrom != 22*60+30 || p.QuietTo != 7*60 || p.AutoHide != 12*time.Second || !p.MutedProjects["github:o/muted"] {
		t.Fatalf("prefs %+v", p)
	}
	evs := []store.Event{
		{Kind: store.EventNewIssue, Repo: "o/a", ItemID: 1},
		{Kind: store.EventNewComment, Repo: "o/a", ItemID: 1},
		{Kind: store.EventNewIssue, Repo: "o/muted", ItemID: 2},
		// The same mod name on another platform is not muted with it (L8).
		{Kind: store.EventNewItem, Repo: "ShareShip", Project: "curseforge:1", ItemID: 3},
		{Kind: store.EventNewItem, Repo: "ShareShip", Project: "nexus:windrose/147", ItemID: 4},
	}
	noon := time.Date(2026, 9, 1, 12, 0, 0, 0, time.Local)
	if got := p.Filter(evs, noon); len(got) != 2 || got[0].ItemID != 1 || got[0].Kind != store.EventNewIssue || got[1].ItemID != 3 {
		t.Fatalf("filter %+v", got)
	}
	if got := p.Filter(evs, noon.Add(11*time.Hour)); len(got) != 0 {
		t.Fatalf("quiet hours let %+v through", got)
	}
	// Toasts ignore quiet hours and the popup switch, not kinds or mutes.
	n.Enabled = false
	if got := liveFilter(n).Filter(evs, noon.Add(11*time.Hour)); len(got) != 2 {
		t.Fatalf("live filter %+v", got)
	}
	n.Quiet.Enabled = false
	if p := notifyPrefs(n); p.QuietFrom != p.QuietTo {
		t.Fatalf("quiet hours off: %d–%d", p.QuietFrom, p.QuietTo)
	}
}

func TestGroupRepeatsKeepsLastPerItem(t *testing.T) {
	evs := []store.Event{
		{Kind: store.EventNewComment, ItemID: 1, Body: "a"},
		{Kind: store.EventNewIssue, ItemID: 2},
		{Kind: store.EventNewComment, ItemID: 1, Body: "b"},
	}
	got := groupRepeats(evs)
	if len(got) != 2 || got[0].ItemID != 2 || got[1].Body != "b" {
		t.Fatalf("grouped %+v", got)
	}
}

func TestPopupThemeFollowsPalette(t *testing.T) {
	a := config.Defaults().Appearance
	a.PaletteID, a.Mode = "rose", "light"
	th := popupTheme(a)
	if th.Accent != hexColor("#b12d5c") || th.Text != hexColor("#2a161f") {
		t.Fatalf("light rose: %+v", th)
	}
	a.Mode = "system"
	if popupTheme(a).Accent != hexColor("#f27fa9") {
		t.Fatal("system mode must use the dark palette")
	}
}
