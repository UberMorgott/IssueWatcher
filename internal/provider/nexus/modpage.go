package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Mod page editing (Phase 7 step 6): the General tab of the site's mod editor
// (next.nexusmods.com /games/<game>/mods/<id>/edit), ported from the MCP
// server's edit_mod_page (src/tools/web-mod.ts:537-603, docs/web-endpoints.md):
//
//	read: api-router `query Mod` (the editor's own query, verbatim) with the
//	      session + GET www /api/flamework/mods/settings?gameId&modId (in the
//	      browser: www is Cloudflare-challenged) → permissions.canEdit, translation
//	save: POST www /api/flamework/mods/save JSON (mapFormDataToSaveParams of the
//	      editor) → {success, modId}; category, author, tags and translation are
//	      resent exactly as loaded.

var _ provider.PageEditor = (*Provider)(nil)

// ErrWriteUnsure: a save was sent without a clear answer; it may be saved
// (check the page before saving again; it is never re-sent).
var ErrWriteUnsure = errWriteUnsure

// modEditQuery is the mod editor's query, verbatim (web-mod.ts MOD_EDIT_QUERY).
const modEditQuery = `
    query Mod($modId: ID!, $gameId: ID!) {
  mod(modId: $modId, gameId: $gameId) {
    author
    description
    game {
      domainName
      id
      name
      supportsVortex
    }
    gameId
    legacyModRequirementsEnabled
    mirrors {
      id
      name
      uri
    }
    modCategory {
      categoryId
      name
    }
    modId
    name
    summary
    tags {
      id
      name
    }
    uid
    uploader {
      avatar
      memberId
      name
    }
    version
  }
}
    `

// Field rules of the editor (edit_mod_page input schema); the summary limit is
// the editor's short-description limit.
const (
	maxPageName    = 250
	maxPageVersion = 255
	maxPageSummary = 350
)

var pageVersionRe = regexp.MustCompile(`^[a-zA-Z0-9.\-]+$`)

// editorMod is the part of the Mod query a save resends.
type editorMod struct {
	Author      *string `json:"author"`
	Description *string `json:"description"`
	GameID      any     `json:"gameId"`
	ModCategory *struct {
		CategoryID any    `json:"categoryId"`
		Name       string `json:"name"`
	} `json:"modCategory"`
	ModID   json.RawMessage `json:"modId"`
	Name    string          `json:"name"`
	Summary *string         `json:"summary"`
	Tags    []struct {
		ID   any    `json:"id"`
		Name string `json:"name"`
	} `json:"tags"`
	Uploader *struct {
		Name *string `json:"name"`
	} `json:"uploader"`
	Version string `json:"version"`
}

// editorSettings is the part of GET /api/flamework/mods/settings a save needs.
type editorSettings struct {
	Permissions *struct {
		CanEdit any `json:"canEdit"`
	} `json:"permissions"`
	Translation *struct {
		Type          any `json:"type"`
		Language      any `json:"language"`
		TranslationOf any `json:"translationOf"`
	} `json:"translation"`
}

// pageTag is one tags[] entry of the save body.
type pageTag struct {
	ID       string `json:"id"`
	Selected bool   `json:"selected"`
}

// pageSaveBody is the exact POST /api/flamework/mods/save body, fields in the
// order the editor's mapFormDataToSaveParams builds them (web-mod.ts:578-593).
type pageSaveBody struct {
	ModID         json.RawMessage `json:"modId"`
	GameID        int64           `json:"gameId"`
	Name          string          `json:"name"`
	Summary       string          `json:"summary"`
	Description   string          `json:"description"`
	CategoryID    int64           `json:"categoryId"`
	Author        string          `json:"author"`
	Version       string          `json:"version"`
	Type          string          `json:"type"`
	LanguageID    int64           `json:"languageId"`
	OriginalModID *int64          `json:"originalModId,omitempty"`
	Tags          []pageTag       `json:"tags"`
	Classtags     []string        `json:"classtags"`
	SaveAllTags   bool            `json:"saveAllTags"`
}

// loadedPage is a page as the editor loads it.
type loadedPage struct {
	game        string
	mod         int
	m           editorMod
	s           editorSettings
	summary     string // decodeBBCode'd
	description string // decodeBBCode'd
}

// native is the site engine; the editor needs it (tests of other parts swap it out).
func (p *Provider) site() (*native, error) {
	n, ok := p.b.(*native)
	if !ok {
		return nil, errors.New("nexus: the mod page editor needs the site engine")
	}
	return n, nil
}

// ModPage implements provider.PageEditor.
func (p *Provider) ModPage(ctx context.Context, project provider.Project) (provider.ModPage, error) {
	n, err := p.site()
	if err != nil {
		return provider.ModPage{}, err
	}
	lp, err := n.loadPage(ctx, project.ExternalID)
	if err != nil {
		return provider.ModPage{}, err
	}
	out := provider.ModPage{Name: lp.m.Name, Summary: lp.summary, Description: lp.description, Version: lp.m.Version,
		Author: lp.author(), Tags: []string{}, URL: fmt.Sprintf("%s/%s/mods/%d", site, lp.game, lp.mod), MaxSummary: maxPageSummary}
	if lp.m.ModCategory != nil {
		out.Category = lp.m.ModCategory.Name
	}
	for _, t := range lp.m.Tags {
		out.Tags = append(out.Tags, t.Name)
	}
	return out, nil
}

// SaveModPage implements provider.PageEditor.
func (p *Provider) SaveModPage(ctx context.Context, project provider.Project, edit provider.ModPageEdit) (provider.ModPageSave, error) {
	if err := checkPageEdit(edit); err != nil {
		return provider.ModPageSave{}, err
	}
	n, err := p.site()
	if err != nil {
		return provider.ModPageSave{}, err
	}
	lp, err := n.loadPage(ctx, project.ExternalID)
	if err != nil {
		return provider.ModPageSave{}, err
	}
	body, err := lp.saveBody(edit)
	if err != nil {
		return provider.ModPageSave{}, err
	}
	raw, err := jsJSON(body)
	if err != nil {
		return provider.ModPageSave{}, err
	}
	u := n.opts.Site + "/api/flamework/mods/save"
	out := provider.ModPageSave{DryRun: edit.DryRun, Changed: lp.changed(edit),
		Request: provider.PublishStep{Method: http.MethodPost, URL: u, Body: json.RawMessage(raw), Note: "mod editor save"}}
	if edit.DryRun {
		return out, nil
	}
	if err := n.saveEditor(ctx, u, n.modPage(lp.game, lp.mod), raw); err != nil {
		return provider.ModPageSave{}, err
	}
	out.Saved = true
	return out, nil
}

// checkPageEdit applies the editor's field rules (no network).
func checkPageEdit(e provider.ModPageEdit) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", provider.ErrBadPageEdit, fmt.Sprintf(format, a...))
	}
	if e.Name != nil && (strings.TrimSpace(*e.Name) == "" || utf8.RuneCountInString(*e.Name) > maxPageName) {
		return bad("name: 1-%d characters", maxPageName)
	}
	if e.Summary != nil && (strings.TrimSpace(*e.Summary) == "" || utf8.RuneCountInString(*e.Summary) > maxPageSummary) {
		return bad("summary: 1-%d characters", maxPageSummary)
	}
	if e.Description != nil && strings.TrimSpace(*e.Description) == "" {
		return bad("description: must not be empty")
	}
	if e.Version != nil && (len(*e.Version) > maxPageVersion || !pageVersionRe.MatchString(*e.Version)) {
		return bad("version: 1-%d characters of letters, digits, . and -", maxPageVersion)
	}
	return nil
}

// loadPage reads the page as the editor does: the Mod query and the settings.
func (n *native) loadPage(ctx context.Context, externalID string) (loadedPage, error) {
	if err := n.signedIn(); err != nil {
		return loadedPage{}, err
	}
	game, mod, err := parseProject(externalID)
	if err != nil {
		return loadedPage{}, err
	}
	gid, err := n.gameID(ctx, game)
	if err != nil {
		return loadedPage{}, err
	}
	var d struct {
		Mod *editorMod `json:"mod"`
	}
	vars := map[string]any{"gameId": strconv.Itoa(gid), "modId": strconv.Itoa(mod)}
	if err := n.graphql(ctx, n.opts.APIRouter, n.opts.Session.Jar(), "Mod", modEditQuery, vars, &d); err != nil {
		return loadedPage{}, err
	}
	if d.Mod == nil {
		return loadedPage{}, fmt.Errorf("%w: mod %s/%d", errNotFound, game, mod)
	}
	r, err := n.opts.Browser.Fetch(ctx, browser.Request{URL: fmt.Sprintf("%s/api/flamework/mods/settings?gameId=%d&modId=%d", n.opts.Site, gid, mod),
		Page: n.modPage(game, mod)})
	if err != nil {
		return loadedPage{}, err
	}
	if r.Status != http.StatusOK {
		if r.Status == http.StatusUnauthorized || r.Status == 419 {
			return loadedPage{}, fmt.Errorf("%w: nexus: mod settings HTTP %d", provider.ErrRelogin, r.Status)
		}
		return loadedPage{}, fmt.Errorf("nexus: mod settings: HTTP %d: %s", r.Status, snippet([]byte(r.Body)))
	}
	var s editorSettings
	if err := json.Unmarshal([]byte(r.Body), &s); err != nil {
		return loadedPage{}, fmt.Errorf("nexus: mod settings: %w", err)
	}
	if s.Permissions == nil || !jsTruthy(s.Permissions.CanEdit) {
		return loadedPage{}, fmt.Errorf("%w: %s/%d (not its author or a team member)", provider.ErrCannotEdit, game, mod)
	}
	lp := loadedPage{game: game, mod: mod, m: *d.Mod, s: s}
	lp.summary = decodeBBCode(deref(d.Mod.Summary))
	lp.description = decodeBBCode(deref(d.Mod.Description))
	return lp, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// author is `m.author ?? m.uploader?.name ?? ""`.
func (lp loadedPage) author() string {
	switch {
	case lp.m.Author != nil:
		return *lp.m.Author
	case lp.m.Uploader != nil && lp.m.Uploader.Name != nil:
		return *lp.m.Uploader.Name
	}
	return ""
}

// saveBody is mapFormDataToSaveParams of the editor (web-mod.ts:576-593).
func (lp loadedPage) saveBody(e provider.ModPageEdit) (pageSaveBody, error) {
	m := lp.m
	gameID, ok := jsInt(m.GameID)
	if !ok {
		return pageSaveBody{}, fmt.Errorf("nexus: mod editor: bad gameId %v", m.GameID)
	}
	if len(m.ModID) == 0 || string(m.ModID) == "null" {
		return pageSaveBody{}, errors.New("nexus: mod editor: no modId")
	}
	var category int64
	if m.ModCategory != nil {
		if category, ok = jsInt(m.ModCategory.CategoryID); !ok {
			return pageSaveBody{}, fmt.Errorf("nexus: mod editor: bad categoryId %v", m.ModCategory.CategoryID)
		}
	}
	// s.translation ?? {type: 1, language: 0, translationOf: 0}
	var trType, trLang, trOf any = float64(1), float64(0), float64(0)
	if t := lp.s.Translation; t != nil {
		trType, trLang, trOf = t.Type, t.Language, t.TranslationOf
	}
	b := pageSaveBody{
		ModID: m.ModID, GameID: gameID, Name: m.Name, Summary: lp.summary, Description: lp.description,
		CategoryID: category, Author: lp.author(), Version: m.Version, Type: "1",
		Tags: []pageTag{}, Classtags: []string{}, SaveAllTags: true,
	}
	if e.Name != nil {
		b.Name = *e.Name
	}
	if e.Summary != nil {
		b.Summary = *e.Summary
	}
	b.Summary = strings.ReplaceAll(b.Summary, "\n", "<br />")
	if e.Description != nil {
		b.Description = *e.Description
	}
	switch {
	case e.Version != nil:
		b.Version = *e.Version
	case m.Version == "":
		b.Version = "1.0"
	}
	if f, isNum := trType.(float64); isNum && f == 2 {
		b.Type = "2"
	}
	if l, ok := jsInt(trLang); ok {
		b.LanguageID = l // Number(tr.language) || 0
	}
	if b.Type == "2" {
		if of, ok := jsInt(trOf); ok && of > 0 {
			b.OriginalModID = &of
		}
	}
	for _, t := range m.Tags {
		id := jsString(t.ID)
		b.Tags = append(b.Tags, pageTag{ID: id, Selected: true})
		b.Classtags = append(b.Classtags, id)
	}
	return b, nil
}

// changed names the fields e changes against the loaded page.
func (lp loadedPage) changed(e provider.ModPageEdit) []string {
	out := []string{}
	if e.Name != nil && *e.Name != lp.m.Name {
		out = append(out, "name")
	}
	if e.Summary != nil && *e.Summary != lp.summary {
		out = append(out, "summary")
	}
	if e.Description != nil && *e.Description != lp.description {
		out = append(out, "description")
	}
	if e.Version != nil && *e.Version != lp.m.Version {
		out = append(out, "version")
	}
	return out
}

// saveEditor sends the save once in the browser (flameworkFetch: JSON,
// credentials included); anything but a clear answer is errWriteUnsure.
func (n *native) saveEditor(ctx context.Context, u, modPage string, body []byte) error {
	r, err := n.opts.Browser.Fetch(ctx, browser.Request{URL: u, Page: modPage, Method: http.MethodPost,
		Headers: map[string]string{"Content-Type": "application/json"}, Body: string(body)})
	if err != nil {
		return fmt.Errorf("%w: POST mods/save: %w", errWriteUnsure, err)
	}
	var j struct {
		Success any    `json:"success"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	decoded := json.Unmarshal([]byte(r.Body), &j) == nil
	switch {
	case r.Status >= 200 && r.Status < 300 && decoded && jsTruthy(j.Success):
		return nil
	case r.Status >= 200 && r.Status < 300 && decoded:
		return fmt.Errorf("nexus: page save refused (success=false): %s", strings.TrimSpace(j.Error+" "+j.Message))
	case refused(r.Status):
		return n.refusal(r)
	}
	return fmt.Errorf("%w: HTTP %d: %s", errWriteUnsure, r.Status, snippet([]byte(r.Body)))
}

// ── the editor's JavaScript semantics ────────────────────────────

var (
	bbEscapedBR = regexp.MustCompile(`(?i)&lt;br\s*/?&gt;`)
	bbBR        = regexp.MustCompile(`(?i)[ \t]*\n?[ \t]*<br\s*/?>[ \t]*\n?[ \t]*`)
	bbNbsp      = regexp.MustCompile(`(?i)&nbsp;`)
	bbEntity    = regexp.MustCompile(`&(#\d+|#[xX][\da-fA-F]+|[0-9a-zA-Z]+);`)
)

// decodeBBCode is the editor's decodeBBCode (web-mod.ts decodeBBCodeInPage):
// escaped and real <br> → \n, &nbsp; → space, then each HTML entity decoded
// as the browser's textarea does.
func decodeBBCode(s string) string {
	if s == "" {
		return ""
	}
	s = bbEscapedBR.ReplaceAllString(s, "<br>")
	s = bbBR.ReplaceAllString(s, "\n")
	s = bbNbsp.ReplaceAllString(s, " ")
	return bbEntity.ReplaceAllStringFunc(s, html.UnescapeString)
}

// jsJSON is JSON.stringify: no HTML escaping of < > &.
func jsJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// jsValueNumber is JavaScript's Number(v) for JSON values.
func jsValueNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		t := strings.TrimSpace(x)
		if t == "" {
			return 0
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	}
	return math.NaN()
}

// jsInt is Number(v) as an integer; false when NaN or fractional.
func jsInt(v any) (int64, bool) {
	f := jsValueNumber(v)
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

// jsString is String(v) for a JSON id.
func jsString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return "null"
	}
	return fmt.Sprint(v)
}

// jsTruthy is JavaScript truthiness for JSON values.
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}
