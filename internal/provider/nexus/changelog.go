package nexus

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Mod changelogs (the mod editor's Documents tab and the Files tab's
// "Edit changelog", next.nexusmods.com /games/<game>/mods/<id>/edit/documents;
// chunks useModDocumentation, useAddChangelogMutation, useEditChangelogMutation
// and the delete-version mutation, all flameworkFetch: POST JSON, credentials
// included, answer {success}):
//
//	list:   GET  www /api/flamework/mods/documentation?gameId&modId
//	        → {readme, changelog: {"<version>": [{change, id, position, version}]} ([] when none)}
//	add:    POST www /api/flamework/mods/changelogs/add {changelogText, gameId, modId, version}
//	edit:   POST www /api/flamework/mods/changelogs/edit {changeIds, changelogText, gameId, modId, version}
//	        (the editor sends every entry id of the version and its text joined by \n)
//	delete: POST www /api/flamework/mods/changelogs/delete-version {changeIds, gameId, modId}
//
// The public v1 API (changelogs.json) has the text only, no ids, and the v3
// API only adds.

var _ provider.ChangelogEditor = (*Provider)(nil)

// maxChangelogVersion is the editor's changelogVersionSchema limit (letters,
// digits, . and - as pageVersionRe).
const maxChangelogVersion = 50

var changelogBR = regexp.MustCompile(`(?i)<br\s*/?>`)

// documentation is the part of GET mods/documentation the changelog needs.
type documentation struct {
	Changelog json.RawMessage `json:"changelog"`
}

type changelogEntry struct {
	Change   string `json:"change"`
	ID       int64  `json:"id"`
	Position int64  `json:"position"`
}

// changelogAdd, changelogEditBody and changelogDelete are the exact bodies,
// fields in the order the editor's object literals build them.
type changelogAdd struct {
	ChangelogText string `json:"changelogText"`
	GameID        int    `json:"gameId"`
	ModID         int    `json:"modId"`
	Version       string `json:"version"`
}

type changelogEditBody struct {
	ChangeIDs     []int64 `json:"changeIds"`
	ChangelogText string  `json:"changelogText"`
	GameID        int     `json:"gameId"`
	ModID         int     `json:"modId"`
	Version       string  `json:"version"`
}

type changelogDelete struct {
	ChangeIDs []int64 `json:"changeIds"`
	GameID    int     `json:"gameId"`
	ModID     int     `json:"modId"`
}

// changelogTarget is a mod the signed-in account may edit.
type changelogTarget struct {
	game     string
	gid, mod int
}

func (n *native) changelogTarget(ctx context.Context, externalID string) (changelogTarget, error) {
	if err := n.signedIn(); err != nil {
		return changelogTarget{}, err
	}
	game, mod, err := parseProject(externalID)
	if err != nil {
		return changelogTarget{}, err
	}
	gid, err := n.gameID(ctx, game)
	if err != nil {
		return changelogTarget{}, err
	}
	if _, err := n.editorSettings(ctx, game, gid, mod); err != nil {
		return changelogTarget{}, err
	}
	return changelogTarget{game: game, gid: gid, mod: mod}, nil
}

// changelogs reads the editor's documentation: versions newest first (the
// editor's order), entries by position.
func (n *native) changelogs(ctx context.Context, t changelogTarget) ([]provider.ChangelogVersion, error) {
	var d documentation
	u := fmt.Sprintf("%s/api/flamework/mods/documentation?gameId=%d&modId=%d", n.opts.Site, t.gid, t.mod)
	if err := n.flameworkGet(ctx, "mod documentation", u, n.modPage(t.game, t.mod), &d); err != nil {
		return nil, err
	}
	return parseChangelog(d.Changelog)
}

// parseChangelog decodes the documentation's changelog record ([] = none).
func parseChangelog(raw json.RawMessage) ([]provider.ChangelogVersion, error) {
	out := []provider.ChangelogVersion{}
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" || strings.HasPrefix(t, "[") {
		return out, nil
	}
	var m map[string][]changelogEntry
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("nexus: mod documentation changelog: %w", err)
	}
	for v, es := range m {
		slices.SortStableFunc(es, func(a, b changelogEntry) int { return cmp.Compare(a.Position, b.Position) })
		cv := provider.ChangelogVersion{Version: v, Entries: []provider.ChangelogEntry{}}
		for _, e := range es {
			cv.Entries = append(cv.Entries, provider.ChangelogEntry{ID: e.ID, Text: changelogBR.ReplaceAllString(e.Change, "\n")})
		}
		out = append(out, cv)
	}
	sortVersionsDesc(out)
	return out, nil
}

// sortVersionsDesc orders versions as the editor does (newest first).
func sortVersionsDesc(vs []provider.ChangelogVersion) {
	slices.SortFunc(vs, func(a, b provider.ChangelogVersion) int { return compareVersionsDesc(a.Version, b.Version) })
}

// compareVersionsDesc is the editor's sort: numeric parts descending, other
// parts by text descending.
func compareVersionsDesc(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		nx, ex := strconv.ParseFloat(x, 64)
		ny, ey := strconv.ParseFloat(y, 64)
		if ex != nil || ey != nil {
			if c := strings.Compare(y, x); c != 0 {
				return c
			}
			continue
		}
		if c := cmp.Compare(ny, nx); c != 0 {
			return c
		}
	}
	return 0
}

// Changelogs implements provider.ChangelogEditor.
func (p *Provider) Changelogs(ctx context.Context, project provider.Project) ([]provider.ChangelogVersion, error) {
	n, err := p.site()
	if err != nil {
		return nil, err
	}
	t, err := n.changelogTarget(ctx, project.ExternalID)
	if err != nil {
		return nil, err
	}
	return n.changelogs(ctx, t)
}

// checkChangelogVersion applies the editor's changelogVersionSchema.
func checkChangelogVersion(v string) error {
	if v == "" || len(v) > maxChangelogVersion || !pageVersionRe.MatchString(v) {
		return fmt.Errorf("%w: version: 1-%d characters of letters, digits, . and -", provider.ErrBadChangelog, maxChangelogVersion)
	}
	return nil
}

// SetChangelog implements provider.ChangelogEditor.
func (p *Provider) SetChangelog(ctx context.Context, project provider.Project, edit provider.ChangelogEdit) (provider.ChangelogSave, error) {
	edit.Version = strings.TrimSpace(edit.Version)
	if err := checkChangelogVersion(edit.Version); err != nil {
		return provider.ChangelogSave{}, err
	}
	want := edit.EditLines()
	if len(want) == 0 {
		return provider.ChangelogSave{}, fmt.Errorf("%w: no changelog lines (use delete to remove a version)", provider.ErrBadChangelog)
	}
	return p.changeChangelog(ctx, project, edit.Version, want, edit.DryRun)
}

// DeleteChangelog implements provider.ChangelogEditor.
func (p *Provider) DeleteChangelog(ctx context.Context, project provider.Project, version string, dryRun bool) (provider.ChangelogSave, error) {
	version = strings.TrimSpace(version)
	if err := checkChangelogVersion(version); err != nil {
		return provider.ChangelogSave{}, err
	}
	return p.changeChangelog(ctx, project, version, nil, dryRun)
}

// changeChangelog makes version read want (nil = deleted) with at most one request.
func (p *Provider) changeChangelog(ctx context.Context, project provider.Project, version string, want []string, dryRun bool) (provider.ChangelogSave, error) {
	n, err := p.site()
	if err != nil {
		return provider.ChangelogSave{}, err
	}
	t, err := n.changelogTarget(ctx, project.ExternalID)
	if err != nil {
		return provider.ChangelogSave{}, err
	}
	current, err := n.changelogs(ctx, t)
	if err != nil {
		return provider.ChangelogSave{}, err
	}
	action, ids, before := provider.PlanChangelog(current, version, want)
	after := want
	if after == nil {
		after = []string{}
	}
	out := provider.ChangelogSave{DryRun: dryRun, Version: version, Action: action, Before: before, After: after}
	if action == provider.ChangelogActionNone {
		out.After = before
		if !dryRun {
			out.Check = p.readBack(ctx, t, current, version, want)
		}
		return out, nil
	}
	out.Changed = true
	text := strings.Join(want, "\n")
	var path string
	var body any
	switch action {
	case provider.ChangelogActionAdd:
		path, body = "changelogs/add", changelogAdd{ChangelogText: text, GameID: t.gid, ModID: t.mod, Version: version}
	case provider.ChangelogActionEdit:
		path, body = "changelogs/edit", changelogEditBody{ChangeIDs: ids, ChangelogText: text, GameID: t.gid, ModID: t.mod, Version: version}
	default:
		path, body = "changelogs/delete-version", changelogDelete{ChangeIDs: ids, GameID: t.gid, ModID: t.mod}
	}
	raw, err := jsJSON(body)
	if err != nil {
		return provider.ChangelogSave{}, err
	}
	u := n.opts.Site + "/api/flamework/mods/" + path
	out.Request = &provider.PublishStep{Method: http.MethodPost, URL: u, Body: json.RawMessage(raw), Note: "mod editor " + action + " changelog"}
	if dryRun {
		return out, nil
	}
	if err := n.saveEditor(ctx, u, n.modPage(t.game, t.mod), raw); err != nil {
		return provider.ChangelogSave{}, err
	}
	out.Saved = true
	out.Check = p.readBack(ctx, t, current, version, want)
	return out, nil
}

// readBack checks the change through v1 changelogs.json (nil without a key
// store). A failed read is reported in the check, never as the change's error.
func (p *Provider) readBack(ctx context.Context, t changelogTarget, current []provider.ChangelogVersion, version string, want []string) *provider.ChangelogCheck {
	if p.opts.Keys == nil {
		return nil
	}
	c, err := p.v1Check(ctx, t, afterChange(current, version, want), version)
	if err != nil {
		p.opts.Log.Warn("nexus: changelog read-back", "project", t.game+"/"+strconv.Itoa(t.mod), "err", err)
	}
	return &c
}
