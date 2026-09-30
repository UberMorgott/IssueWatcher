package modkit_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
)

func TestPageChanged(t *testing.T) {
	var st provider.PollState
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	if !modkit.PageChanged(&st, "k", "a", now, time.Hour) {
		t.Fatal("no fingerprint yet must reconcile")
	}
	if modkit.PageChanged(&st, "k", "a", now.Add(time.Minute), time.Hour) {
		t.Fatal("same fingerprint")
	}
	if !modkit.PageChanged(&st, "k", "b", now.Add(2*time.Minute), time.Hour) {
		t.Fatal("changed fingerprint")
	}
	if modkit.PageChanged(&st, "k", "b", now.Add(61*time.Minute), time.Hour) {
		t.Fatal("reconciled 59 min ago")
	}
	if !modkit.PageChanged(&st, "k", "b", now.Add(63*time.Minute), time.Hour) {
		t.Fatal("last reconcile over an hour ago")
	}
}

func TestHTTPS(t *testing.T) {
	const fb = "https://fallback/"
	for in, want := range map[string]string{
		"https://www.curseforge.com/hytale/mods/x": "https://www.curseforge.com/hytale/mods/x",
		"javascript:alert(1)//https://x":           fb,
		" JavaScript:alert(1)":                     fb,
		"http://www.curseforge.com/x":              fb,
		"data:text/html,x":                         fb,
		"//evil.example/x":                         fb,
		"":                                         fb,
	} {
		if got := modkit.HTTPS(in, fb); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
}

func TestTitle(t *testing.T) {
	if got := modkit.Title("\n  hello world \nsecond"); got != "hello world" {
		t.Fatal(got)
	}
	long := ""
	for range 100 {
		long += "a"
	}
	if got := []rune(modkit.Title(long)); len(got) != 80 {
		t.Fatalf("len %d", len(got))
	}
}

// A fingerprint missing or taken before the last full read is adopted as the
// baseline (that read stored everything); a legacy fingerprint of unknown age
// still reconciles.
func TestPageChangedAdoptsAfterFullRead(t *testing.T) {
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	st := provider.PollState{FullAt: now}
	if modkit.PageChanged(&st, "k", "a", now.Add(time.Minute), time.Hour) {
		t.Fatal("no fingerprint yet after a full read must be adopted")
	}
	st.FullAt = now.Add(2 * time.Minute) // a reconcile after that check
	if modkit.PageChanged(&st, "k", "b", now.Add(3*time.Minute), time.Hour) {
		t.Fatal("a fingerprint older than the full read must be adopted")
	}
	if !modkit.PageChanged(&st, "k", "c", now.Add(4*time.Minute), time.Hour) {
		t.Fatal("a change after the full read must reconcile")
	}
	legacy := provider.PollState{ETags: map[string]string{"k": "a", "k@full": strconv.FormatInt(now.Unix(), 10)}}
	if !modkit.PageChanged(&legacy, "k", "b", now.Add(time.Minute), time.Hour) {
		t.Fatal("a legacy fingerprint (age unknown) that changed must reconcile")
	}
	if _, ok := legacy.ETags["k@full"]; ok || !legacy.FullAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("legacy state not migrated: %+v", legacy)
	}
}
