package provider

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// Editing a mod's per-version changelog from the app (Nexus first): a
// PageEditor that also implements ChangelogEditor.

// ErrBadChangelog: a changelog edit the platform would refuse (checked before sending).
var ErrBadChangelog = errors.New("bad changelog edit")

// ChangelogEditor lists, replaces and deletes a mod's changelog entries.
type ChangelogEditor interface {
	// Changelogs lists every version's entries as the site's editor loads them.
	Changelogs(ctx context.Context, project Project) ([]ChangelogVersion, error)
	// SetChangelog replaces all entries of edit.Version with edit.Lines (one
	// entry per line), or adds them when the version has none. Equal entries
	// send nothing. DryRun builds the request and sends nothing.
	SetChangelog(ctx context.Context, project Project, edit ChangelogEdit) (ChangelogSave, error)
	// DeleteChangelog deletes every entry of version (none: nothing is sent).
	DeleteChangelog(ctx context.Context, project Project, version string, dryRun bool) (ChangelogSave, error)
}

// ChangelogEntry is one changelog line.
type ChangelogEntry struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
}

// ChangelogVersion is one version's entries.
type ChangelogVersion struct {
	Version string           `json:"version"`
	Entries []ChangelogEntry `json:"entries"`
}

// ChangelogEdit is a replace request for one version.
type ChangelogEdit struct {
	Version string   `json:"version"`
	Lines   []string `json:"lines,omitempty"`
	// Text is the alternative to Lines: one entry per non-empty line.
	Text   string `json:"text,omitempty"`
	DryRun bool   `json:"dryRun,omitempty"`
}

// Changelog actions.
const (
	ChangelogActionNone   = "none"   // the version already reads so (set) or has no entries (delete)
	ChangelogActionAdd    = "add"    // the version had no entries
	ChangelogActionEdit   = "edit"   // all entries of the version replaced
	ChangelogActionDelete = "delete" // all entries of the version deleted
)

// ChangelogSave is the outcome of a set or delete (or its plan when DryRun).
type ChangelogSave struct {
	DryRun  bool     `json:"dryRun,omitempty"`
	Version string   `json:"version"`
	Action  string   `json:"action"`
	Changed bool     `json:"changed"`
	Before  []string `json:"before"`
	After   []string `json:"after"`
	// Request is the one request sent (nil when Action is none).
	Request *PublishStep `json:"request,omitempty"`
	Saved   bool         `json:"saved,omitempty"`
}

// ChangelogLines splits text into entries: one per non-empty line, trimmed,
// a leading "- " or "* " bullet dropped (the site draws its own bullets),
// a bare bullet skipped.
func ChangelogLines(text string) []string {
	out := []string{}
	for l := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		for _, b := range []string{"- ", "* "} {
			l = strings.TrimSpace(strings.TrimPrefix(l, b))
		}
		if l != "" && l != "-" && l != "*" {
			out = append(out, l)
		}
	}
	return out
}

// EditLines are the edit's entries: Lines (each normalized as ChangelogLines
// does) or else Text split into lines.
func (e ChangelogEdit) EditLines() []string {
	if len(e.Lines) == 0 {
		return ChangelogLines(e.Text)
	}
	return ChangelogLines(strings.Join(e.Lines, "\n"))
}

// PlanChangelog decides how to make version read want (nil want = delete it):
// the action and the ids of the version's current entries, which an edit or a
// delete replaces as a whole, so a version never ends up with duplicates.
func PlanChangelog(current []ChangelogVersion, version string, want []string) (action string, ids []int64, before []string) {
	before = []string{}
	for _, v := range current {
		if v.Version != version {
			continue
		}
		for _, e := range v.Entries {
			ids = append(ids, e.ID)
			before = append(before, strings.TrimSpace(e.Text))
		}
	}
	switch {
	case want == nil && len(ids) == 0:
		return ChangelogActionNone, nil, before
	case want == nil:
		return ChangelogActionDelete, ids, before
	case len(ids) == 0:
		return ChangelogActionAdd, nil, before
	case slices.Equal(before, want):
		return ChangelogActionNone, ids, before
	}
	return ChangelogActionEdit, ids, before
}
