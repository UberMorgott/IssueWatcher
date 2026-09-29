// Package nexus is the Nexus Mods provider over the owner's MCP server
// (E:\DEV\nexusmods-mcp-server, `format:"json"`): projects = the mods of an
// author, items = Posts root threads (kind comment) and bug reports (kind bug).
package nexus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Platform is the provider id.
const Platform = "nexus"

const site = "https://www.nexusmods.com"

// Caller is the bridge side the provider needs (*mcpbridge.Client).
type Caller interface {
	Call(ctx context.Context, tool string, args map[string]any, out any, read bool) error
}

// Options configures the provider.
type Options struct {
	Bridge Caller
	// Author returns the mod author name to list (the mod's author field, e.g.
	// "Morgott"; the uploader account may differ).
	Author func() string
	Log    *slog.Logger
	Now    func() time.Time
	// ReadBackWaits are the pauses before each further read-back of a reply
	// whose id is unknown (default 5, 10, 15 s: new posts show up late).
	ReadBackWaits []time.Duration
}

// Provider implements provider.Provider and provider.Poller.
type Provider struct {
	opts Options

	mu      sync.Mutex
	account string              // uploader name of the author's mods (= comment author name)
	bugs    map[string]bugCache // bug id → last read report, re-read when its list row changes
}

type bugCache struct {
	stamp string // replies + lastPostAt of the list row it was read with
	item  provider.Item
}

// New creates the provider.
func New(opts Options) *Provider {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReadBackWaits == nil {
		opts.ReadBackWaits = []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second}
	}
	if opts.Author == nil {
		opts.Author = func() string { return "" }
	}
	return &Provider{opts: opts, bugs: map[string]bugCache{}}
}

// Platform implements provider.Provider.
func (p *Provider) Platform() string { return Platform }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ListProjects: true, SyncItems: true, ListComments: true, Reply: canReply,
		Auth: provider.AuthCookieSession, Kinds: []string{store.KindComment, store.KindBug}, ReplyThreaded: true,
	}
}

// Scheduling implements provider.Poller: a browser-backed site, polled gently.
func (p *Provider) Scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: 15 * time.Minute, RateBudget: 60}
}

func (p *Provider) call(ctx context.Context, tool string, args map[string]any, out any) error {
	return mcpbridge.ProviderError(p.opts.Bridge.Call(ctx, tool, args, out, true), p.opts.Now())
}

// ── projects ─────────────────────────────────────────────────────

type modsResult struct {
	Total int `json:"total"`
	Mods  []struct {
		Game     string `json:"game"`
		ModID    int    `json:"modId"`
		Name     string `json:"name"`
		URL      string `json:"url"`
		Uploader struct {
			Name *string `json:"name"`
		} `json:"uploader"`
	} `json:"mods"`
}

// Account implements provider.Provider: reads need no login, so the account
// is the uploader name of the author's mods (what comments show as author).
func (p *Provider) Account(ctx context.Context) (string, error) {
	author := strings.TrimSpace(p.opts.Author())
	if author == "" {
		return "", fmt.Errorf("%w: nexus: no mod author set (providers.nexus.author)", provider.ErrNotSignedIn)
	}
	var r modsResult
	if err := p.call(ctx, "search_mods", map[string]any{"author": author, "count": 1}, &r); err != nil {
		if errors.Is(err, mcpbridge.ErrUnavailable) {
			return "", fmt.Errorf("%w: %w", provider.ErrNotSignedIn, err)
		}
		return "", err
	}
	name := author
	if len(r.Mods) > 0 && r.Mods[0].Uploader.Name != nil && *r.Mods[0].Uploader.Name != "" {
		name = *r.Mods[0].Uploader.Name
	}
	p.mu.Lock()
	p.account = name
	p.mu.Unlock()
	return name, nil
}

// ListProjects implements provider.Provider: every mod of the author. A
// failed page fails the listing (a partial one would deactivate projects).
func (p *Provider) ListProjects(ctx context.Context) ([]provider.Project, error) {
	author := strings.TrimSpace(p.opts.Author())
	var out []provider.Project
	for offset := 0; ; {
		var r modsResult
		if err := p.call(ctx, "search_mods", map[string]any{"author": author, "count": 50, "offset": offset}, &r); err != nil {
			return nil, err
		}
		for _, m := range r.Mods {
			url := m.URL
			if url == "" {
				url = fmt.Sprintf("%s/%s/mods/%d", site, m.Game, m.ModID)
			}
			out = append(out, provider.Project{ExternalID: fmt.Sprintf("%s/%d", m.Game, m.ModID), Name: m.Name, URL: url})
		}
		offset += len(r.Mods)
		if len(r.Mods) == 0 || offset >= r.Total {
			return out, nil
		}
	}
}

// parseProject splits "<game>/<modId>".
func parseProject(id string) (string, int, error) {
	game, mod, ok := strings.Cut(id, "/")
	n, err := strconv.Atoi(mod)
	if !ok || game == "" || err != nil || n <= 0 {
		return "", 0, fmt.Errorf("nexus: bad project id %q", id)
	}
	return game, n, nil
}

// ── items ────────────────────────────────────────────────────────

type post struct {
	ID        string  `json:"id"`
	Author    string  `json:"author"`
	CreatedAt *string `json:"createdAt"`
	Body      string  `json:"body"`
}

type thread struct {
	post
	Sticky  bool   `json:"sticky"`
	Locked  bool   `json:"locked"`
	Replies []post `json:"replies"`
}

type commentsResult struct {
	Page     int      `json:"page"`
	Pages    int      `json:"pages"`
	Total    int      `json:"total"`
	Comments []thread `json:"comments"`
}

type bugRow struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Status     string  `json:"status"`
	StatusKey  *string `json:"statusKey"`
	Open       bool    `json:"open"`
	Replies    int     `json:"replies"`
	LastPostAt *string `json:"lastPostAt"`
}

type bugsResult struct {
	Page  int      `json:"page"`
	Pages int      `json:"pages"`
	URL   string   `json:"url"`
	Bugs  []bugRow `json:"bugs"`
}

type bugPost struct {
	ID             string  `json:"id"`
	Author         string  `json:"author"`
	CreatedAt      *string `json:"createdAt"`
	CreatedAtLocal *string `json:"createdAtLocal"`
	Body           string  `json:"body"`
}

type bugResult struct {
	Report  bugPost   `json:"report"`
	Replies []bugPost `json:"replies"`
}

// maxPages bounds a listing (a broken "pages" never loops forever).
const maxPages = 200

// SyncItems implements provider.Provider: every Posts thread and bug report
// (all pages, deduplicated by id; since is not used — the site has no
// "changed since" filter and a new reply can land on any page).
func (p *Provider) SyncItems(ctx context.Context, project provider.Project, _ time.Time) ([]provider.Item, error) {
	game, mod, err := parseProject(project.ExternalID)
	if err != nil {
		return nil, err
	}
	items, err := p.comments(ctx, game, mod)
	if err != nil {
		return nil, err
	}
	bugs, err := p.bugItems(ctx, game, mod)
	if err != nil {
		return nil, err
	}
	items = append(items, bugs...)
	slices.SortStableFunc(items, func(a, b provider.Item) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	return items, nil
}

func (p *Provider) comments(ctx context.Context, game string, mod int) ([]provider.Item, error) {
	seen := map[string]bool{}
	var out []provider.Item
	for page := 1; page <= maxPages; page++ {
		var r commentsResult
		err := p.call(ctx, "get_mod_comments", map[string]any{"game": game, "mod_id": mod, "page": page}, &r)
		if mcpbridge.IsCode(err, mcpbridge.CodeDisabled) {
			return nil, nil // no Posts tab
		}
		if err != nil {
			return nil, fmt.Errorf("comments page %d: %w", page, err)
		}
		for _, t := range r.Comments {
			if seen[t.ID] { // pages shifted by a new thread: already read
				continue
			}
			seen[t.ID] = true
			out = append(out, threadItem(game, mod, t))
		}
		if page >= r.Pages || len(r.Comments) == 0 {
			break
		}
	}
	return out, nil
}

func commentURL(game string, mod int, id string) string {
	return fmt.Sprintf("%s/%s/mods/%d?tab=posts&jump_to_comment=%s", site, game, mod, id)
}

func threadItem(game string, mod int, t thread) provider.Item {
	created := mcpbridge.Time(t.CreatedAt)
	it := provider.Item{
		ExternalID: commentExternalID(game, mod, t.ID), Kind: store.KindComment, Number: mcpbridge.Number(t.ID),
		Title: mcpbridge.Title(t.Body), Body: t.Body, URL: commentURL(game, mod, t.ID), Author: t.Author,
		Open: true, CreatedAt: created, UpdatedAt: created,
	}
	for _, r := range t.Replies {
		at := mcpbridge.Time(r.CreatedAt)
		it.UpdatedAt = mcpbridge.Latest(it.UpdatedAt, at)
		it.Comments = append(it.Comments, provider.Comment{ExternalID: r.ID, Author: r.Author, Body: r.Body,
			URL: commentURL(game, mod, r.ID), CreatedAt: at, UpdatedAt: at})
	}
	return it
}

// commentExternalID carries the mod: Reply gets only the item's external id.
func commentExternalID(game string, mod int, id string) string {
	return fmt.Sprintf("comment:%s/%d/%s", game, mod, id)
}

func bugExternalID(id string) string { return "bug:" + id }

func (p *Provider) bugItems(ctx context.Context, game string, mod int) ([]provider.Item, error) {
	seen := map[string]bool{}
	var out []provider.Item
	for page := 1; page <= maxPages; page++ {
		var r bugsResult
		err := p.call(ctx, "get_mod_bugs", map[string]any{"game": game, "mod_id": mod, "page": page}, &r)
		if mcpbridge.IsCode(err, mcpbridge.CodeDisabled) {
			return nil, nil // Bugs tab off
		}
		if err != nil {
			return nil, fmt.Errorf("bugs page %d: %w", page, err)
		}
		for _, b := range r.Bugs {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			it, ok, err := p.bug(ctx, game, mod, r.URL, b)
			if err != nil {
				return nil, fmt.Errorf("bug %s: %w", b.ID, err)
			}
			if ok {
				out = append(out, it)
			}
		}
		if page >= r.Pages || len(r.Bugs) == 0 {
			break
		}
	}
	return out, nil
}

// bug returns one bug report with its replies; ok=false when it vanished.
// The report is re-read only when its list row (replies, last post) changed.
func (p *Provider) bug(ctx context.Context, game string, mod int, listURL string, b bugRow) (provider.Item, bool, error) {
	last := mcpbridge.Time(b.LastPostAt)
	stamp := fmt.Sprintf("%d|%s|%s|%s", b.Replies, last.Format(time.RFC3339), b.Status, b.Title)
	p.mu.Lock()
	c, ok := p.bugs[b.ID]
	p.mu.Unlock()
	if ok && c.stamp == stamp {
		return c.item, true, nil
	}
	var r bugResult
	err := p.call(ctx, "get_mod_bug", map[string]any{"issue_id": mcpbridge.Number(b.ID)}, &r)
	if mcpbridge.IsCode(err, mcpbridge.CodeNotFound) {
		return provider.Item{}, false, nil
	}
	if err != nil {
		return provider.Item{}, false, err
	}
	if listURL == "" {
		listURL = fmt.Sprintf("%s/%s/mods/%d?tab=bugs", site, game, mod)
	}
	status := b.Status
	if b.StatusKey != nil && *b.StatusKey != "" {
		status = *b.StatusKey
	}
	// Bug posts carry only a profile-local time; the list's lastPostAt is UTC.
	created := localTime(r.Report.CreatedAtLocal)
	if created.IsZero() || (len(r.Replies) == 0 && !last.IsZero()) {
		created = last
	}
	it := provider.Item{
		ExternalID: bugExternalID(b.ID), Kind: store.KindBug, Number: mcpbridge.Number(b.ID),
		Title: b.Title, Body: r.Report.Body, URL: listURL, Author: r.Report.Author,
		Open: b.Open, RawStatus: status, CreatedAt: created, UpdatedAt: mcpbridge.Latest(created, last),
	}
	if !b.Open {
		it.ClosedAt = it.UpdatedAt
	}
	for i, rp := range r.Replies {
		at := localTime(rp.CreatedAtLocal)
		if i == len(r.Replies)-1 && !last.IsZero() {
			at = last
		}
		it.Comments = append(it.Comments, provider.Comment{ExternalID: rp.ID, Author: rp.Author, Body: rp.Body,
			URL: listURL, CreatedAt: at, UpdatedAt: at})
	}
	p.mu.Lock()
	p.bugs[b.ID] = bugCache{stamp: stamp, item: it}
	p.mu.Unlock()
	return it, true, nil
}

// localTime parses the site's profile-local "YYYY-MM-DDTHH:MM" in the local zone.
func localTime(s *string) time.Time {
	if s == nil || *s == "" {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, *s, time.Local); err == nil {
			return t.UTC()
		}
	}
	return mcpbridge.Time(s)
}

// ── change check ─────────────────────────────────────────────────

const sigKey = "nexus:page1"

// DetectChanges implements provider.Poller: page 1 of Posts and Bugs against
// the last fingerprint; a difference asks for a reconcile of the project.
func (p *Provider) DetectChanges(ctx context.Context, project provider.Project, st *provider.PollState) (provider.Changes, error) {
	var ch provider.Changes
	game, mod, err := parseProject(project.ExternalID)
	if err != nil {
		return ch, err
	}
	parts := []string{}
	var c commentsResult
	ch.Requests++
	err = p.call(ctx, "get_mod_comments", map[string]any{"game": game, "mod_id": mod, "page": 1}, &c)
	switch {
	case mcpbridge.IsCode(err, mcpbridge.CodeDisabled):
	case err != nil:
		return ch, err
	default:
		parts = append(parts, strconv.Itoa(c.Total), strconv.Itoa(c.Pages))
		for _, t := range c.Comments {
			parts = append(parts, t.ID+":"+strconv.Itoa(len(t.Replies)))
		}
	}
	var b bugsResult
	ch.Requests++
	err = p.call(ctx, "get_mod_bugs", map[string]any{"game": game, "mod_id": mod, "page": 1}, &b)
	switch {
	case mcpbridge.IsCode(err, mcpbridge.CodeDisabled):
	case err != nil:
		return ch, err
	default:
		parts = append(parts, "bugs", strconv.Itoa(b.Pages))
		for _, r := range b.Bugs {
			parts = append(parts, fmt.Sprintf("%s:%d:%s:%s", r.ID, r.Replies, r.Status, mcpbridge.Time(r.LastPostAt).Format(time.RFC3339)))
		}
	}
	sig := mcpbridge.Signature(parts...)
	if st.ETags == nil {
		st.ETags = map[string]string{}
	}
	prev := st.ETags[sigKey]
	st.ETags[sigKey] = sig
	// No fingerprint yet: the reconcile that created the target just read everything.
	ch.Overflow = prev != "" && prev != sig
	return ch, nil
}

// FetchChanged implements provider.Poller (the change check never lists
// numbers: it asks for a reconcile).
func (p *Provider) FetchChanged(ctx context.Context, project provider.Project, _ []int) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, time.Time{})
}

// FullReconcile implements provider.Poller.
func (p *Provider) FullReconcile(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, since)
}
