package provider

import (
	"slices"
	"testing"
)

func TestChangelogLines(t *testing.T) {
	got := ChangelogLines("- Fixed: a  \r\n\r\n* Added: b\n   \nChanged: c\n-  d")
	if want := []string{"Fixed: a", "Added: b", "Changed: c", "d"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %q", got)
	}
	if got := (ChangelogEdit{Lines: []string{"- x", "", " y "}, Text: "ignored"}).EditLines(); !slices.Equal(got, []string{"x", "y"}) {
		t.Fatalf("edit lines = %q", got)
	}
	if got := ChangelogLines("\n \n"); got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v", got)
	}
}

// A set replaces the version's entries as a whole (all ids, one request), adds
// a missing version, sends nothing for equal lines and never touches other versions.
func TestPlanChangelog(t *testing.T) {
	cur := []ChangelogVersion{
		{Version: "0.2.9", Entries: []ChangelogEntry{{ID: 90, Text: "x"}}},
		{Version: "0.2.4", Entries: []ChangelogEntry{{ID: 41, Text: "old a"}, {ID: 42, Text: "old b "}, {ID: 43, Text: "old a"}}},
	}
	for _, tc := range []struct {
		version string
		want    []string
		action  string
		ids     []int64
		before  []string
	}{
		{"0.2.4", []string{"new"}, ChangelogActionEdit, []int64{41, 42, 43}, []string{"old a", "old b", "old a"}},
		{"0.2.4", []string{"old a", "old b", "old a"}, ChangelogActionNone, []int64{41, 42, 43}, []string{"old a", "old b", "old a"}},
		{"0.2.4", []string{"old b", "old a", "old a"}, ChangelogActionEdit, []int64{41, 42, 43}, []string{"old a", "old b", "old a"}},
		{"0.2.3", []string{"new"}, ChangelogActionAdd, nil, []string{}},
		{"0.2.4", nil, ChangelogActionDelete, []int64{41, 42, 43}, []string{"old a", "old b", "old a"}},
		{"0.2.3", nil, ChangelogActionNone, nil, []string{}},
	} {
		action, ids, before := PlanChangelog(cur, tc.version, tc.want)
		if action != tc.action || !slices.Equal(ids, tc.ids) || !slices.Equal(before, tc.before) {
			t.Errorf("%s %q: %s %v %q", tc.version, tc.want, action, ids, before)
		}
		if slices.Contains(ids, 90) {
			t.Errorf("%s: touches 0.2.9", tc.version)
		}
	}
}
