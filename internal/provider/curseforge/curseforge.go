// Package curseforge is the CurseForge provider over the owner's MCP server
// (E:\DEV\curseforge, `format:"json"`): projects = the author's projects,
// items = root comment threads (kind comment) with their nested replies.
// The site needs the server's session cookies even to read.
package curseforge

import (
	"context"
	"errors"
	"fmt"
	"html"
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
const Platform = "curseforge"

// ErrRelogin means the stored session expired or was refused (Cloudflare):
// the owner must sign in again from Settings; the sync never opens a login window.
var ErrRelogin = errors.New("curseforge: re-login needed")

// ErrUnknownOutcome: the post may or may not have landed and a read-back did
// not find it.
var ErrUnknownOutcome = errors.New("исход неизвестен — проверьте страницу")

// Caller is the bridge side the provider needs (*mcpbridge.Client).
type Caller interface {
	Call(ctx context.Context, tool string, args map[string]any, out any, read bool) error
}

// Options configures the provider.
type Options struct {
	Bridge Caller
	// Author overrides the CFWidget author name whose projects are listed
	// (default: the session's display name).
	Author func() string
	Log    *slog.Logger
	Now    func() time.Time
	// ReadBackWaits are the pauses before each further read-back of a reply
	// whose id is unknown (default 5, 10 s).
	ReadBackWaits []time.Duration
}

// Provider implements provider.Provider and provider.Poller.
type Provider struct {
	opts Options

	mu      sync.Mutex
	account string            // display name = comment author name
	urls    map[string]string // project id → page URL (get_project, cached)
}

// New creates the provider.
func New(opts Options) *Provider {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Author == nil {
		opts.Author = func() string { return "" }
	}
	if opts.ReadBackWaits == nil {
		opts.ReadBackWaits = []time.Duration{5 * time.Second, 10 * time.Second}
	}
	return &Provider{opts: opts, urls: map[string]string{}}
}

// Platform implements provider.Provider.
func (p *Provider) Platform() string { return Platform }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ListProjects: true, SyncItems: true, ListComments: true, Reply: true,
		Auth: provider.AuthCookieSession, Kinds: []string{store.KindComment}, ReplyThreaded: true,
	}
}

// Scheduling implements provider.Poller: a browser-backed site, polled gently.
func (p *Provider) Scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: 15 * time.Minute, RateBudget: 60}
}

// call runs a read and maps session problems: not_logged_in / cloudflare →
// ErrRelogin (an error state, never "no new comments").
func (p *Provider) call(ctx context.Context, tool string, args map[string]any, out any) error {
	err := p.opts.Bridge.Call(ctx, tool, args, out, true)
	if mcpbridge.IsCode(err, mcpbridge.CodeNotLoggedIn) || mcpbridge.IsCode(err, mcpbridge.CodeCloudflare) {
		return fmt.Errorf("%w: %w", ErrRelogin, err)
	}
	return mcpbridge.ProviderError(err, p.opts.Now())
}

type sessionResult struct {
	LoggedIn      bool   `json:"loggedIn"`
	CookiesStored bool   `json:"cookiesStored"`
	Detail        string `json:"detail"`
	User          *struct {
		DisplayName *string `json:"displayName"`
		Username    *string `json:"username"`
	} `json:"user"`
}

// Account implements provider.Provider: the session's display name (what
// comments show as author). No cookies → signed out; stored cookies refused →
// ErrRelogin; server missing → signed out (unavailable).
func (p *Provider) Account(ctx context.Context) (string, error) {
	var s sessionResult
	err := p.call(ctx, "cf_session_status", nil, &s)
	switch {
	case errors.Is(err, mcpbridge.ErrUnavailable):
		return "", fmt.Errorf("%w: %w", provider.ErrNotSignedIn, err)
	case err != nil:
		return "", err
	case !s.LoggedIn && !s.CookiesStored:
		return "", fmt.Errorf("%w: curseforge: no session (%s)", provider.ErrNotSignedIn, s.Detail)
	case !s.LoggedIn:
		return "", fmt.Errorf("%w (%s)", ErrRelogin, s.Detail)
	}
	name := ""
	if s.User != nil {
		for _, v := range []*string{s.User.DisplayName, s.User.Username} {
			if v != nil && *v != "" {
				name = *v
				break
			}
		}
	}
	if name == "" {
		return "", fmt.Errorf("%w: curseforge: session has no user", provider.ErrNotSignedIn)
	}
	p.mu.Lock()
	p.account = name
	p.mu.Unlock()
	return name, nil
}

func (p *Provider) self(ctx context.Context) (string, error) {
	p.mu.Lock()
	a := p.account
	p.mu.Unlock()
	if a != "" {
		return a, nil
	}
	return p.Account(ctx)
}

// ListProjects implements provider.Provider (CFWidget, keyless).
func (p *Provider) ListProjects(ctx context.Context) ([]provider.Project, error) {
	author := strings.TrimSpace(p.opts.Author())
	if author == "" {
		a, err := p.self(ctx)
		if err != nil {
			return nil, err
		}
		author = a
	}
	var r struct {
		Projects []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := p.call(ctx, "search_author", map[string]any{"username": author}, &r); err != nil {
		return nil, err
	}
	out := make([]provider.Project, 0, len(r.Projects))
	for _, pr := range r.Projects {
		id := strconv.Itoa(pr.ID)
		out = append(out, provider.Project{ExternalID: id, Name: pr.Name, URL: p.projectURL(ctx, pr.ID)})
	}
	return out, nil
}

// projectURL is the project's page (get_project once per id; a failure
// falls back to the id redirect).
func (p *Provider) projectURL(ctx context.Context, id int) string {
	key := strconv.Itoa(id)
	p.mu.Lock()
	u, ok := p.urls[key]
	p.mu.Unlock()
	if ok {
		return u
	}
	var r struct {
		URL *string `json:"url"`
	}
	u = "https://www.curseforge.com/projects/" + key
	if err := p.call(ctx, "get_project", map[string]any{"project": key}, &r); err == nil && r.URL != nil && *r.URL != "" {
		u = *r.URL
		p.mu.Lock()
		p.urls[key] = u
		p.mu.Unlock()
	}
	return u
}

// ── items ────────────────────────────────────────────────────────

type comment struct {
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Author    string  `json:"author"`
	CreatedAt *string `json:"createdAt"`
	UpdatedAt *string `json:"updatedAt"`
	Body      string  `json:"body"`
}

type thread struct {
	comment
	Pinned  bool      `json:"pinned"`
	Replies []comment `json:"replies"`
}

type commentsResult struct {
	Page     int      `json:"page"`
	Pages    *int     `json:"pages"`
	Total    *int     `json:"total"`
	Comments []thread `json:"comments"`
}

const maxPages = 200

func modID(project string) (int, error) {
	n, err := strconv.Atoi(project)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("curseforge: bad project id %q", project)
	}
	return n, nil
}

func (p *Provider) page(ctx context.Context, mod, page int) (commentsResult, error) {
	var r commentsResult
	err := p.call(ctx, "get_comments", map[string]any{"mod_id": mod, "page": page}, &r)
	return r, err
}

// SyncItems implements provider.Provider: every root thread, all pages (a new
// reply stays on its thread's old page), deduplicated by id.
func (p *Provider) SyncItems(ctx context.Context, project provider.Project, _ time.Time) ([]provider.Item, error) {
	mod, err := modID(project.ExternalID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var items []provider.Item
	for n := 1; n <= maxPages; n++ {
		r, err := p.page(ctx, mod, n)
		if err != nil {
			return nil, fmt.Errorf("comments page %d: %w", n, err)
		}
		for _, t := range r.Comments {
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			items = append(items, threadItem(project, mod, t))
		}
		if len(r.Comments) == 0 || (r.Pages != nil && n >= *r.Pages) {
			break
		}
	}
	slices.SortStableFunc(items, func(a, b provider.Item) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	return items, nil
}

func externalID(mod int, id string) string { return fmt.Sprintf("comment:%d/%s", mod, id) }

func threadItem(project provider.Project, mod int, t thread) provider.Item {
	url := project.URL
	if url == "" {
		url = fmt.Sprintf("https://www.curseforge.com/projects/%d", mod)
	}
	url = strings.TrimSuffix(url, "/") + "/comments"
	created := mcpbridge.Time(t.CreatedAt)
	it := provider.Item{
		ExternalID: externalID(mod, t.ID), Kind: store.KindComment, Number: mcpbridge.Number(t.ID),
		Title: mcpbridge.Title(t.Body), Body: t.Body, URL: url, Author: t.Author, Open: true,
		CreatedAt: created, UpdatedAt: mcpbridge.Latest(created, mcpbridge.Time(t.UpdatedAt)),
	}
	for _, r := range t.Replies {
		c := mcpbridge.Time(r.CreatedAt)
		u := mcpbridge.Latest(c, mcpbridge.Time(r.UpdatedAt))
		it.UpdatedAt = mcpbridge.Latest(it.UpdatedAt, u)
		it.Comments = append(it.Comments, provider.Comment{ExternalID: r.ID, Author: r.Author, Body: r.Body,
			URL: url, CreatedAt: c, UpdatedAt: u})
	}
	return it
}

// ── change check ─────────────────────────────────────────────────

const sigKey = "curseforge:page1"

// DetectChanges implements provider.Poller: page 1 ids + the entry total (a
// reply on an old thread should raise the total) against the last
// fingerprint; no fingerprint yet or FullEvery elapsed also reconciles.
func (p *Provider) DetectChanges(ctx context.Context, project provider.Project, st *provider.PollState) (provider.Changes, error) {
	ch := provider.Changes{Requests: 1}
	mod, err := modID(project.ExternalID)
	if err != nil {
		return ch, err
	}
	r, err := p.page(ctx, mod, 1)
	if err != nil {
		return ch, err
	}
	parts := []string{}
	if r.Total != nil {
		parts = append(parts, strconv.Itoa(*r.Total))
	}
	for _, t := range r.Comments {
		parts = append(parts, t.ID+":"+strconv.Itoa(len(t.Replies)))
	}
	ch.Overflow = mcpbridge.PageChanged(st, sigKey, mcpbridge.Signature(parts...), p.opts.Now(), FullEvery)
	return ch, nil
}

// FullEvery bounds how long page 1 alone is trusted (a reply on a thread past
// page 1 changes page 1 only through the total, which is not verified live).
const FullEvery = time.Hour

// FetchChanged implements provider.Poller (unused: the check asks for a reconcile).
func (p *Provider) FetchChanged(ctx context.Context, project provider.Project, _ []int) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, time.Time{})
}

// FullReconcile implements provider.Poller.
func (p *Provider) FullReconcile(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, since)
}

// ── reply ────────────────────────────────────────────────────────

// readBackPages bounds the pages searched for a lost reply (new replies stay
// on their thread's page; the server itself reads back the first 3).
const readBackPages = 10

// readBackTimeout bounds the read-back of a reply whose outcome is unknown; it
// runs even when the request that posted it was cancelled.
const readBackTimeout = 2 * time.Minute

// readBackSkew is how much older than the send time a found reply may look
// (clock differences) and still count as the one just posted.
const readBackSkew = 5 * time.Minute

// htmlBody turns a plain-text reply into the RawHtml the site stores: markup
// characters escaped, line breaks kept.
func htmlBody(text string) string {
	s := html.EscapeString(strings.ReplaceAll(text, "\r\n", "\n"))
	return strings.ReplaceAll(s, "\n", "<br>")
}

// Reply implements provider.Provider: post_comment{reply_to_id} on the root
// comment (the plain-text body is sent as escaped HTML). Never retried; a lost
// or failed-after-send answer is resolved by reading the thread back,
// ignoring the replies that existed before the post.
func (p *Provider) Reply(ctx context.Context, itemExternalID, body string) (provider.Comment, error) {
	rest, ok := strings.CutPrefix(itemExternalID, "comment:")
	ms, root, ok2 := strings.Cut(rest, "/")
	mod, err := modID(ms)
	if !ok || !ok2 || err != nil || mcpbridge.Number(root) <= 0 {
		return provider.Comment{}, fmt.Errorf("curseforge: bad item id %q", itemExternalID)
	}
	account, err := p.self(ctx)
	if err != nil {
		return provider.Comment{}, err
	}
	// The replies already there: an older reply with the same text is never
	// taken for this one. A failed read sends nothing.
	before := map[string]bool{}
	if err := p.eachReply(ctx, mod, root, func(c comment) bool { before[c.ID] = true; return false }); err != nil {
		return provider.Comment{}, fmt.Errorf("curseforge: read thread before reply: %w", err)
	}
	sent := p.opts.Now()
	var r struct {
		Posted bool    `json:"posted"`
		ID     *string `json:"id"`
	}
	err = p.opts.Bridge.Call(ctx, "post_comment", map[string]any{"mod_id": mod, "comment_text": htmlBody(body), "reply_to_id": mcpbridge.Number(root)}, &r, false)
	switch {
	case err == nil && r.Posted && r.ID != nil && *r.ID != "":
		return p.posted(*r.ID, account, body), nil
	case err == nil && !r.Posted:
		return provider.Comment{}, errors.New("curseforge: post_comment: not posted")
	case mcpbridge.IsCode(err, mcpbridge.CodeNotLoggedIn), mcpbridge.IsCode(err, mcpbridge.CodeCloudflare):
		return provider.Comment{}, fmt.Errorf("%w: %w", provider.ErrNotSignedIn, err)
	case err != nil && !mcpbridge.WriteUnsure(err):
		return provider.Comment{}, mcpbridge.ProviderError(err, p.opts.Now())
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBackTimeout)
	defer cancel()
	var id string
	var ferr error
	for _, wait := range append([]time.Duration{0}, p.opts.ReadBackWaits...) {
		if !sleep(rctx, wait) {
			break
		}
		if id, ferr = p.findReply(rctx, mod, root, account, body, before, sent); ferr == nil && id != "" {
			return p.posted(id, account, body), nil
		}
	}
	p.opts.Log.Warn("curseforge: reply outcome unknown", "item", itemExternalID, "err", err, "readback", ferr)
	return provider.Comment{}, fmt.Errorf("curseforge: %w", ErrUnknownOutcome)
}

func (p *Provider) posted(id, account, body string) provider.Comment {
	now := p.opts.Now().UTC()
	return provider.Comment{ExternalID: id, Author: account, Body: body, CreatedAt: now, UpdatedAt: now}
}

// eachReply calls fn with every reply under root on the first readBackPages
// pages that list it, until fn returns true.
func (p *Provider) eachReply(ctx context.Context, mod int, root string, fn func(comment) bool) error {
	for n := 1; n <= readBackPages; n++ {
		r, err := p.page(ctx, mod, n)
		if err != nil {
			return err
		}
		found := false
		for _, t := range r.Comments {
			if t.ID != root {
				continue
			}
			found = true
			for _, c := range t.Replies {
				if fn(c) {
					return nil
				}
			}
		}
		if found || len(r.Comments) == 0 || (r.Pages != nil && n >= *r.Pages) {
			return nil
		}
	}
	return nil
}

// findReply looks for the newest own reply with the same text under root that
// was not there before the post and is not older than the send.
func (p *Provider) findReply(ctx context.Context, mod int, root, account, body string, before map[string]bool, sent time.Time) (string, error) {
	best, bestN := "", -1
	err := p.eachReply(ctx, mod, root, func(c comment) bool {
		created := mcpbridge.Time(c.CreatedAt)
		if !before[c.ID] && c.Author == account && sameReply(c.Body, body) && mcpbridge.Number(c.ID) > bestN &&
			(created.IsZero() || !created.Before(sent.Add(-readBackSkew))) {
			best, bestN = c.ID, mcpbridge.Number(c.ID)
		}
		return false
	})
	return best, err
}

// sameReply compares the site's text of a reply with the plain text sent. The
// site may prefix replies with "In reply to X:" and may return markup.
func sameReply(got, sent string) bool {
	g := dropReplyPrefix(got)
	return mcpbridge.SameText(g, sent) || mcpbridge.SameText(html.UnescapeString(stripTags(g)), sent)
}

func dropReplyPrefix(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(t), "in reply to ") {
		if _, after, ok := strings.Cut(t, ":"); ok {
			t = after
		}
	}
	return strings.TrimSpace(t)
}

// stripTags replaces tags with spaces (<br>, <p> separate words).
func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
			b.WriteByte(' ')
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
