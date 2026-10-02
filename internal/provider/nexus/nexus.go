// Package nexus is the Nexus Mods provider (native engine: v2 GraphQL, the site
// GraphQL, in-page browser fetches): projects = the mods of an author, items =
// Posts root threads (kind comment) and bug reports (kind bug).
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
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Platform is the provider id.
const Platform = "nexus"

const site = "https://www.nexusmods.com"

// Options configures the provider.
type Options struct {
	// Native configures the engine (nil = defaults).
	Native *NativeOptions
	// Author returns the uploader account whose mods are listed: its exact
	// account name (e.g. "UberMorgott") or member id. Not the mod's free-text
	// author field, which anyone can set to any name.
	Author func() string
	Log    *slog.Logger
	Now    func() time.Time
	// ReadBackWaits are the pauses before each further read-back of a reply
	// whose id is unknown (default 5, 10, 15 s: new posts show up late).
	ReadBackWaits []time.Duration
	// Keys is the API key store; with it the provider publishes new versions
	// over the v3 API (provider.Publisher, publish.go).
	Keys *Keys
	// V3 overrides the v3 client (tests: fake API and storage); its Key
	// defaults to Keys.Key.
	V3 *V3Options
}

// Provider implements provider.Provider and provider.Poller.
type Provider struct {
	opts Options
	b    backend
	v3   *v3 // nil: publishing off
	// writes serialized per thread (one reply at a time on an item).
	threads sync.Map // item external id → *sync.Mutex

	mu      sync.Mutex
	account string              // uploader account name (= comment author name)
	member  *uploader           // the signed-in member, when no account is configured
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
	p := &Provider{opts: opts, bugs: map[string]bugCache{}}
	var no NativeOptions
	if opts.Native != nil {
		no = *opts.Native
	}
	p.b = newNative(no, opts.Log, opts.Now)
	if opts.Keys != nil || opts.V3 != nil {
		var vo V3Options
		if opts.V3 != nil {
			vo = *opts.V3
		}
		if vo.Key == nil && opts.Keys != nil {
			vo.Key = opts.Keys.Key
		}
		if vo.Version == "" && opts.Keys != nil {
			vo.Version = opts.Keys.opts.Version
		}
		p.v3 = newV3(vo)
	}
	return p
}

// Platform implements provider.Provider.
func (p *Provider) Platform() string { return Platform }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities {
	_, site := p.b.(*native) // the mod page editor goes through the site (modpage.go)
	return provider.Capabilities{
		ListProjects: true, SyncItems: true, ListComments: true, Reply: canReply,
		Auth: provider.AuthCookieSession, Kinds: []string{store.KindComment, store.KindBug}, ReplyThreaded: true, Publish: p.v3 != nil,
		EditPage: site,
	}
}

// Scheduling implements provider.Poller: a browser-backed site, polled gently.
func (p *Provider) Scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: 15 * time.Minute, RateBudget: 60}
}

// ── projects ─────────────────────────────────────────────────────

type modsResult struct {
	Total int      `json:"total"`
	Mods  []modRow `json:"mods"`
}

type modRow struct {
	Game     string `json:"game"`
	ModID    int    `json:"modId"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Uploader struct {
		Name     *string `json:"name"`
		MemberID *int    `json:"memberId"`
	} `json:"uploader"`
	codeURL string // GitHub repo the description links (native engine)
}

// uploader is the configured account: an exact uploader account name or its
// member id. Both identify one account (the mod's free-text author field does
// not: anyone can type any name there).
type uploader struct {
	name string
	id   int
}

// uploader is the configured account, else the signed-in member (cached).
func (p *Provider) uploader(ctx context.Context) (uploader, error) {
	v := strings.TrimSpace(p.opts.Author())
	if v == "" {
		p.mu.Lock()
		m := p.member
		p.mu.Unlock()
		if m != nil {
			return *m, nil
		}
		l, err := p.LoginStatus(ctx)
		switch {
		case err != nil:
			return uploader{}, err
		case !l.LoggedIn:
			return uploader{}, fmt.Errorf("%w: nexus: not signed in (Settings › Платформы › Подключить)", provider.ErrNotSignedIn)
		}
		p.mu.Lock()
		m = p.member
		p.mu.Unlock()
		if m == nil {
			return uploader{}, fmt.Errorf("nexus: signed in, but the account is unknown (%s)", l.Detail)
		}
		return *m, nil
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return uploader{id: n}, nil
	}
	return uploader{name: v}, nil
}

// owns reports whether a listed mod was uploaded by the account.
func (u uploader) owns(name *string, id *int) bool {
	if u.id > 0 {
		return id != nil && *id == u.id
	}
	return name != nil && strings.EqualFold(*name, u.name)
}

// errNotOwner: the listing ignored the uploader filter and listed other
// people's mods (never watched).
var errNotOwner = errors.New("nexus: the mod listing ignored the uploader filter")

// Account implements provider.Provider: the configured uploader account, else
// the signed-in member of the stored session,
// checked against its mods; the name is what comments show as author.
func (p *Provider) Account(ctx context.Context) (string, error) {
	u, err := p.uploader(ctx)
	if err != nil {
		return "", err
	}
	r, err := p.b.searchMods(ctx, u, 1, 0)
	if err != nil {
		return "", err
	}
	name := cmpName(u.name, p.memberName(u.id))
	if len(r.Mods) > 0 {
		m := r.Mods[0]
		if !u.owns(m.Uploader.Name, m.Uploader.MemberID) {
			return "", errNotOwner
		}
		if m.Uploader.Name != nil && *m.Uploader.Name != "" {
			name = *m.Uploader.Name
		}
	}
	if name == "" {
		return "", fmt.Errorf("%w: nexus: member %d has no mods to learn the account name from", provider.ErrNotSignedIn, u.id)
	}
	p.mu.Lock()
	p.account = name
	p.mu.Unlock()
	return name, nil
}

// ListProjects implements provider.Provider: every mod the account uploaded.
// A failed page fails the listing (a partial one would deactivate projects).
func (p *Provider) ListProjects(ctx context.Context) ([]provider.Project, error) {
	u, err := p.uploader(ctx)
	if err != nil {
		return nil, err
	}
	var out []provider.Project
	for offset := 0; ; {
		r, err := p.b.searchMods(ctx, u, 50, offset)
		if err != nil {
			return nil, err
		}
		for _, m := range r.Mods {
			if !u.owns(m.Uploader.Name, m.Uploader.MemberID) {
				return nil, errNotOwner
			}
			url := modkit.HTTPS(m.URL, fmt.Sprintf("%s/%s/mods/%d", site, m.Game, m.ModID))
			out = append(out, provider.Project{ExternalID: fmt.Sprintf("%s/%d", m.Game, m.ModID), Name: m.Name, URL: url, CodeURL: m.codeURL})
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
		r, err := p.b.comments(ctx, game, mod, page)
		if errors.Is(err, errDisabled) {
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
	created := modkit.Time(t.CreatedAt)
	it := provider.Item{
		ExternalID: commentExternalID(game, mod, t.ID), Kind: store.KindComment, Number: modkit.Number(t.ID),
		Title: modkit.Title(t.Body), Body: t.Body, URL: commentURL(game, mod, t.ID), Author: t.Author,
		Open: true, CreatedAt: created, UpdatedAt: created,
	}
	for _, r := range t.Replies {
		at := modkit.Time(r.CreatedAt)
		it.UpdatedAt = modkit.Latest(it.UpdatedAt, at)
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
		r, err := p.b.bugs(ctx, game, mod, page)
		if errors.Is(err, errDisabled) {
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
	last := modkit.Time(b.LastPostAt)
	stamp := fmt.Sprintf("%d|%s|%s|%s", b.Replies, last.Format(time.RFC3339), b.Status, b.Title)
	p.mu.Lock()
	c, ok := p.bugs[b.ID]
	p.mu.Unlock()
	if ok && c.stamp == stamp {
		return c.item, true, nil
	}
	r, err := p.b.bug(ctx, modkit.Number(b.ID))
	if errors.Is(err, errNotFound) {
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
		ExternalID: bugExternalID(b.ID), Kind: store.KindBug, Number: modkit.Number(b.ID),
		Title: b.Title, Body: r.Report.Body, URL: listURL, Author: r.Report.Author,
		Open: b.Open, RawStatus: status, CreatedAt: created, UpdatedAt: modkit.Latest(created, last),
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
	return modkit.Time(s)
}

// ── change check ─────────────────────────────────────────────────

// DetectChanges implements provider.Poller: page 1 of Posts and Bugs against
// the last fingerprint; a difference, a missing fingerprint (restart, new
// target: a comment may have landed since the last full read) or FullEvery
// elapsed asks for a reconcile of the project.
func (p *Provider) DetectChanges(ctx context.Context, project provider.Project, st *provider.PollState) (provider.Changes, error) {
	var ch provider.Changes
	game, mod, err := parseProject(project.ExternalID)
	if err != nil {
		return ch, err
	}
	parts := []string{}
	ch.Requests++
	c, err := p.b.comments(ctx, game, mod, 1)
	switch {
	case errors.Is(err, errDisabled):
	case err != nil:
		return ch, err
	default:
		parts = append(parts, strconv.Itoa(c.Total), strconv.Itoa(c.Pages))
		for _, t := range c.Comments {
			parts = append(parts, t.ID+":"+strconv.Itoa(len(t.Replies)))
		}
	}
	ch.Requests++
	b, err := p.b.bugs(ctx, game, mod, 1)
	switch {
	case errors.Is(err, errDisabled):
	case err != nil:
		return ch, err
	default:
		parts = append(parts, "bugs", strconv.Itoa(b.Pages))
		for _, r := range b.Bugs {
			parts = append(parts, fmt.Sprintf("%s:%d:%s:%s", r.ID, r.Replies, r.Status, modkit.Time(r.LastPostAt).Format(time.RFC3339)))
		}
	}
	ch.Overflow = modkit.PageChanged(st, nativeSigV2, modkit.Signature(parts...), p.opts.Now(), FullEvery)
	return ch, nil
}

// FullEvery bounds how long page 1 alone is trusted: whether the site's
// comment total counts replies is unverified, so a reply on a thread past
// page 1 may leave page 1 unchanged; the project is re-read at least this often.
const FullEvery = time.Hour

// FetchChanged implements provider.Poller (the change check never lists
// numbers: it asks for a reconcile).
func (p *Provider) FetchChanged(ctx context.Context, project provider.Project, _ []int) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, time.Time{})
}

// FullReconcile implements provider.Poller.
func (p *Provider) FullReconcile(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, since)
}

// ── sign-in ──────────────────────────────────────────────────────

func (p *Provider) remember(m *uploader) {
	if m == nil {
		return
	}
	p.mu.Lock()
	p.member = m
	p.mu.Unlock()
}

// Login implements provider.Loginer: the stored session, the signed-in
// profile, else the sign-in window.
func (p *Provider) Login(ctx context.Context) (provider.Login, error) {
	l, m, err := p.b.login(ctx)
	if err != nil {
		return provider.Login{}, err
	}
	p.remember(m)
	return l, nil
}

// LoginStatus implements provider.Loginer (session + member).
func (p *Provider) LoginStatus(ctx context.Context) (provider.Login, error) {
	l, m, err := p.b.loginStatus(ctx)
	if err != nil {
		return provider.Login{}, err
	}
	p.remember(m)
	return l, nil
}

// Logout implements provider.Logouter: the stored session is dropped (public
// reads go on).
func (p *Provider) Logout(ctx context.Context) error {
	if err := p.b.logout(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	p.member = nil
	p.account = ""
	p.mu.Unlock()
	return nil
}

// CancelLogin implements provider.LoginCanceller.
func (p *Provider) CancelLogin(ctx context.Context) error { return p.b.cancelLogin(ctx) }

// Member is the signed-in member id (0 = unknown), for storing it as the account.
func (p *Provider) Member() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.member == nil {
		return 0
	}
	return p.member.id
}

func (p *Provider) memberName(id int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.member != nil && p.member.id == id && id > 0 {
		return p.member.name
	}
	return ""
}

func cmpName(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
