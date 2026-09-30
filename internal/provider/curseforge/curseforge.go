// Package curseforge is the CurseForge provider (native engine: the site's
// own JSON API, CFWidget, the stored session): projects = the author's
// projects, items = root comment threads (kind comment) with their nested
// replies.
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
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Platform is the provider id.
const Platform = "curseforge"

// ErrRelogin means the stored session expired or was refused (Cloudflare):
// the owner must sign in again from Settings; the sync never opens a login window.
var ErrRelogin = fmt.Errorf("curseforge: re-login needed (%w)", provider.ErrRelogin)

// ErrUnknownOutcome: the post may or may not have landed and a read-back did
// not find it.
var ErrUnknownOutcome = modkit.ErrUnknownOutcome

// Options configures the provider.
type Options struct {
	// Native configures the engine (nil = defaults).
	Native *NativeOptions
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
	b    backend
	// writes serialized per thread.
	threads sync.Map // item external id → *sync.Mutex

	mu      sync.Mutex
	account string            // display name = comment author name
	urls    map[string]string // project id → page URL (get_project, cached)
	codes   map[string]string // project id → GitHub repo its summary links ("" = none)
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
	p := &Provider{opts: opts, urls: map[string]string{}, codes: map[string]string{}}
	var no NativeOptions
	if opts.Native != nil {
		no = *opts.Native
	}
	p.b = newNative(no, opts.Log, opts.Now)
	return p
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

// Scheduling implements provider.Poller.
func (p *Provider) Scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: 5 * time.Minute, RateBudget: 120}
}

type sessionResult struct {
	LoggedIn      bool   `json:"loggedIn"`
	CookiesStored bool   `json:"cookiesStored"`
	InProgress    bool   `json:"loginInProgress"`
	Detail        string `json:"detail"`
	User          *struct {
		DisplayName *string `json:"displayName"`
		Username    *string `json:"username"`
	} `json:"user"`
	Source     *string `json:"sessionSource"`
	Browser    *string `json:"sessionBrowser"`
	Via        *string `json:"loginVia"`
	ViaBrowser *string `json:"loginBrowser"`
}

// Account implements provider.Provider: the session's display name (what
// comments show as author). No cookies → signed out, except with a configured
// author: its reads are keyless (CFWidget by name, plain comments), so that
// name is the account (replies still need the session: post checks it);
// stored cookies refused → ErrRelogin.
func (p *Provider) Account(ctx context.Context) (string, error) {
	s, err := p.b.status(ctx)
	switch {
	case err != nil:
		return "", err
	case !s.LoggedIn && !s.CookiesStored:
		if a := strings.TrimSpace(p.opts.Author()); a != "" {
			return a, nil // not cached as the signed-in account; a reply's post still needs the session
		}
		return "", fmt.Errorf("%w: curseforge: no session (%s)", provider.ErrNotSignedIn, s.Detail)
	case !s.LoggedIn:
		return "", fmt.Errorf("%w (%s)", ErrRelogin, s.Detail)
	}
	name := s.name()
	if name == "" {
		return "", fmt.Errorf("%w: curseforge: session has no user", provider.ErrNotSignedIn)
	}
	p.mu.Lock()
	p.account = name
	p.mu.Unlock()
	return name, nil
}

// name is the session's display name (else username).
func (s sessionResult) name() string {
	if s.User != nil {
		for _, v := range []*string{s.User.DisplayName, s.User.Username} {
			if v != nil && *v != "" {
				return *v
			}
		}
	}
	return ""
}

// Login implements provider.Loginer: the stored session, our signed-in
// profile, else the sign-in window.
func (p *Provider) Login(ctx context.Context) (provider.Login, error) {
	l, err := p.b.login(ctx)
	if err == nil && l.LoggedIn && l.Account != "" {
		p.mu.Lock()
		p.account = l.Account
		p.mu.Unlock()
	}
	return l, err
}

// CancelLogin implements provider.LoginCanceller.
func (p *Provider) CancelLogin(ctx context.Context) error { return p.b.cancelLogin(ctx) }

// LoginStatus implements provider.Loginer.
func (p *Provider) LoginStatus(ctx context.Context) (provider.Login, error) {
	l, err := p.b.loginStatus(ctx)
	if err != nil {
		return provider.Login{}, err
	}
	if l.LoggedIn {
		p.mu.Lock()
		p.account = l.Account
		p.mu.Unlock()
	}
	return l, nil
}

// Logout implements provider.Logouter: the stored session is dropped.
func (p *Provider) Logout(ctx context.Context) error {
	if err := p.b.logout(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	p.account = ""
	p.mu.Unlock()
	return nil
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
	r, err := p.b.author(ctx, author)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Project, 0, len(r.Projects))
	for _, pr := range r.Projects {
		id := strconv.Itoa(pr.ID)
		u := p.projectURL(ctx, pr.ID)
		p.mu.Lock()
		code := p.codes[id]
		p.mu.Unlock()
		out = append(out, provider.Project{ExternalID: id, Name: pr.Name, URL: u, CodeURL: code})
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
	u = "https://www.curseforge.com/projects/" + key
	if r, err := p.b.project(ctx, id); err == nil && r.URL != nil && *r.URL != "" {
		u = modkit.HTTPS(*r.URL, u) // CFWidget data: never a javascript: href
		p.mu.Lock()
		p.urls[key] = u
		p.codes[key] = provider.GitHubRepoURL(r.Summary)
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
	return p.b.page(ctx, mod, page)
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
	created := modkit.Time(t.CreatedAt)
	it := provider.Item{
		ExternalID: externalID(mod, t.ID), Kind: store.KindComment, Number: modkit.Number(t.ID),
		Title: modkit.Title(t.Body), Body: t.Body, URL: url, Author: t.Author, Open: true,
		CreatedAt: created, UpdatedAt: modkit.Latest(created, modkit.Time(t.UpdatedAt)),
	}
	for _, r := range t.Replies {
		c := modkit.Time(r.CreatedAt)
		u := modkit.Latest(c, modkit.Time(r.UpdatedAt))
		it.UpdatedAt = modkit.Latest(it.UpdatedAt, u)
		it.Comments = append(it.Comments, provider.Comment{ExternalID: r.ID, Author: r.Author, Body: r.Body,
			URL: url, CreatedAt: c, UpdatedAt: u})
	}
	return it
}

// ── change check ─────────────────────────────────────────────────

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
		var part strings.Builder
		part.WriteString(t.ID + ":" + strconv.Itoa(len(t.Replies)))
		// An edit changes the modification time only.
		part.WriteString(":" + modkit.Latest(modkit.Time(t.CreatedAt), modkit.Time(t.UpdatedAt)).Format(time.RFC3339))
		for _, r := range t.Replies {
			part.WriteString("," + modkit.Latest(modkit.Time(r.CreatedAt), modkit.Time(r.UpdatedAt)).Format(time.RFC3339))
		}
		parts = append(parts, part.String())
	}
	ch.Overflow = modkit.PageChanged(st, sigKey, modkit.Signature(parts...), p.opts.Now(), FullEvery)
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
// on their thread's page).
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
	if !ok || !ok2 || err != nil || modkit.Number(root) <= 0 {
		return provider.Comment{}, fmt.Errorf("curseforge: bad item id %q", itemExternalID)
	}
	account, err := p.self(ctx)
	if err != nil {
		return provider.Comment{}, err
	}
	lock, _ := p.threads.LoadOrStore(itemExternalID, &sync.Mutex{})
	mu, _ := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	// The replies already there: an older reply with the same text is never
	// taken for this one. A failed read sends nothing.
	before := map[string]bool{}
	if err := p.eachReply(ctx, mod, root, func(c comment) bool { before[c.ID] = true; return false }); err != nil {
		return provider.Comment{}, fmt.Errorf("curseforge: read thread before reply: %w", err)
	}
	sent := p.opts.Now()
	r, err := p.b.post(ctx, mod, htmlBody(body), modkit.Number(root))
	switch {
	case err == nil && r.Posted && r.ID != nil && *r.ID != "":
		return p.posted(*r.ID, account, body), nil
	case err == nil && !r.Posted:
		return provider.Comment{}, errors.New("curseforge: post_comment: not posted")
	case err != nil && !errors.Is(err, errWriteUnsure):
		return provider.Comment{}, err
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
			if slices.ContainsFunc(t.Replies, fn) {
				return nil
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
		created := modkit.Time(c.CreatedAt)
		if !before[c.ID] && c.Author == account && sameReply(c.Body, body) && modkit.Number(c.ID) > bestN &&
			(created.IsZero() || !created.Before(sent.Add(-readBackSkew))) {
			best, bestN = c.ID, modkit.Number(c.ID)
		}
		return false
	})
	return best, err
}

// sameReply compares the site's text of a reply with the plain text sent. The
// site may prefix replies with "In reply to X:" and may return markup.
func sameReply(got, sent string) bool {
	g := dropReplyPrefix(got)
	return modkit.SameText(g, sent) || modkit.SameText(html.UnescapeString(stripTags(g)), sent)
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
