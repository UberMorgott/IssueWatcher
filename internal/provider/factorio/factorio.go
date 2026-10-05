// Package factorio is the Factorio Mod Portal provider (docs/ARCHITECTURE.md
// → Native mod platforms): projects = the mods of a portal user, items = the
// discussion threads of each mod (kind bug for the Bugs category, else
// comment), comments = the thread's later messages. Reads are keyless plain
// HTTP; the sign-in (cookies from the browser window) is needed only to reply.
package factorio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// Platform is the provider id.
const Platform = "factorio"

const (
	site      = "https://mods.factorio.com"
	signInURL = "https://mods.factorio.com/login"
	sigKey    = "v2:factorio:page1"
)

// canReply stays off until one owner-authorised post and its read-back pass
// (the signed-in reply form is unverified: TASKS.md Phase 6 step 11/15).
const canReply = false

// FullEvery is how often a project is swept in full (edits, deletes and
// category moves do not change the list order the delta relies on).
const FullEvery = time.Hour

// maxPages bounds a listing.
const maxPages = 200

// Origins are the https origins of the sign-in window.
var Origins = []string{site, "https://factorio.com"}

// Options configures the provider.
type Options struct {
	HTTP    *http.Client
	Session *signin.Manager // nil = keyless reads only
	// Author is the portal username whose mods are listed ("" = the signed-in one).
	Author func() string
	Log    *slog.Logger
	Now    func() time.Time
	// Site overrides https://mods.factorio.com (tests).
	Site string
	// ReadBackWaits are the pauses before each further read-back of a reply.
	ReadBackWaits []time.Duration
	// Keys is the upload API key store; nil = no publishing.
	Keys *Keys
}

// Provider implements provider.Provider, provider.Poller and (with Options.Keys) provider.Publisher.
type Provider struct {
	opts Options

	mu      sync.Mutex
	full    map[string]time.Time // project → last full sweep (this run)
	threads sync.Map             // item id → *sync.Mutex (one write at a time)
	mods    sync.Map             // thread id → mod name (read this run)
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
	if opts.Site == "" {
		opts.Site = site
	}
	if opts.ReadBackWaits == nil {
		opts.ReadBackWaits = []time.Duration{5 * time.Second, 10 * time.Second}
	}
	return &Provider{opts: opts, full: map[string]time.Time{}}
}

// Platform implements provider.Provider.
func (p *Provider) Platform() string { return Platform }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ListProjects: true, SyncItems: true, ListComments: true, Reply: canReply,
		Auth: provider.AuthCookieSession, Kinds: []string{store.KindComment, store.KindBug}, ReplyThreaded: true,
		Publish: p.opts.Keys != nil,
	}
}

// Scheduling implements provider.Poller: one plain request per change check.
func (p *Provider) Scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: 5 * time.Minute, RateBudget: 120}
}

func (p *Provider) jar() *websession.Jar {
	if p.opts.Session == nil {
		return nil
	}
	return p.opts.Session.Jar()
}

func (p *Provider) client(jar *websession.Jar) *websession.Client {
	u, _ := url.Parse(p.opts.Site)
	return &websession.Client{Jar: jar, Hosts: []string{u.Hostname()}, HTTP: p.opts.HTTP}
}

// get reads a portal page (anonymous unless jar).
func (p *Provider) get(ctx context.Context, path string, jar *websession.Jar) (string, error) {
	res, err := p.client(jar).Do(ctx, http.MethodGet, p.opts.Site+path, nil, nil, jar == nil)
	switch {
	case err != nil:
		return "", fmt.Errorf("factorio: %s: %w", path, err)
	case res.Status == http.StatusTooManyRequests:
		return "", &provider.RateLimitError{Reset: p.opts.Now().Add(15 * time.Minute)}
	case res.Status == http.StatusNotFound:
		return "", fmt.Errorf("factorio: %s: %w", path, errNotFound)
	case res.Status != http.StatusOK:
		return "", fmt.Errorf("factorio: %s: HTTP %d", path, res.Status)
	}
	return string(res.Body), nil
}

var errNotFound = errors.New("not found")

// ── account & projects ───────────────────────────────────────────

// Account implements provider.Provider: the configured portal username, else
// the signed-in one.
func (p *Provider) Account(context.Context) (string, error) {
	if a := strings.TrimSpace(p.opts.Author()); a != "" {
		return a, nil
	}
	if p.opts.Session != nil {
		if l := p.opts.Session.Status(); l.LoggedIn && l.Account != "" {
			return l.Account, nil
		}
	}
	return "", fmt.Errorf("%w: factorio: no portal user (Settings › Платформы › Подключить)", provider.ErrNotSignedIn)
}

// ListProjects implements provider.Provider: every mod on the user's page,
// with the GitHub repository its source_url names.
func (p *Provider) ListProjects(ctx context.Context) ([]provider.Project, error) {
	user, err := p.Account(ctx)
	if err != nil {
		return nil, err
	}
	var refs []modRef
	for page, last := 1, 1; page <= last && page <= maxPages; page++ {
		path := "/user/" + url.PathEscape(user)
		if page > 1 {
			path += fmt.Sprintf("?page=%d", page)
		}
		body, err := p.get(ctx, path, nil)
		if err != nil {
			return nil, err
		}
		mods, lp, err := parseUserMods(body)
		if err != nil {
			return nil, err
		}
		refs, last = append(refs, mods...), lp
	}
	out := make([]provider.Project, 0, len(refs))
	for _, m := range refs {
		pr := provider.Project{ExternalID: m.Name, Name: m.Title, URL: site + "/mod/" + url.PathEscape(m.Name), Game: Platform}
		var info struct {
			Title     string `json:"title"`
			SourceURL string `json:"source_url"`
			Homepage  string `json:"homepage"`
		}
		if body, err := p.get(ctx, "/api/mods/"+url.PathEscape(m.Name)+"/full", nil); err == nil && json.Unmarshal([]byte(body), &info) == nil {
			if info.Title != "" {
				pr.Name = info.Title
			}
			pr.CodeURL = provider.GitHubRepoURL(info.SourceURL + " " + info.Homepage)
		}
		out = append(out, pr)
	}
	return out, nil
}

// ── items ────────────────────────────────────────────────────────

func threadURL(mod, id string) string {
	return site + "/mod/" + url.PathEscape(mod) + "/discussion/" + id
}

func itemID(id string) string { return "thread:" + id }

// listPage reads one discussion list page.
func (p *Provider) listPage(ctx context.Context, mod string, page int) ([]row, int, error) {
	path := "/mod/" + url.PathEscape(mod) + "/discussion"
	if page > 1 {
		path += fmt.Sprintf("/page/%d", page)
	}
	body, err := p.get(ctx, path, nil)
	if err != nil {
		return nil, 0, err
	}
	return parseList(body)
}

// thread reads a thread into an item (ok=false when it is gone).
func (p *Provider) thread(ctx context.Context, mod string, r row) (provider.Item, bool, error) {
	body, err := p.get(ctx, "/mod/"+url.PathEscape(mod)+"/discussion/"+r.ID, nil)
	if errors.Is(err, errNotFound) {
		return provider.Item{}, false, nil
	}
	if err != nil {
		return provider.Item{}, false, err
	}
	msgs, _, err := parseThread(body)
	if err != nil {
		return provider.Item{}, false, err
	}
	return threadItem(mod, r, msgs), true, nil
}

// threadItem builds the item: the first message is the body, the rest are
// comments with external id <thread>/<time> (+ "#n" on a collision).
func threadItem(mod string, r row, msgs []message) provider.Item {
	kind := store.KindComment
	if strings.EqualFold(r.Category, "Bugs") {
		kind = store.KindBug
	}
	u := threadURL(mod, r.ID)
	it := provider.Item{ExternalID: itemID(r.ID), Kind: kind, Title: r.Title, URL: u, Author: r.Author, Open: true,
		RawStatus: r.Category, CreatedAt: parseTime(r.Posted), UpdatedAt: parseTime(r.Last)}
	if it.Title == "" {
		it.Title = "(untitled)"
	}
	seen := map[string]int{}
	for i, m := range msgs {
		at := parseTime(m.At)
		if i == 0 {
			it.Body = m.Body
			if m.Author != "" {
				it.Author = m.Author
			}
			if !at.IsZero() {
				it.CreatedAt = at
			}
			continue
		}
		id := r.ID + "/" + m.At
		seen[id]++
		if n := seen[id]; n > 1 {
			id = fmt.Sprintf("%s#%d", id, n)
		}
		it.Comments = append(it.Comments, provider.Comment{ExternalID: id, Author: m.Author, Body: m.Body, URL: u, CreatedAt: at, UpdatedAt: at})
		it.UpdatedAt = modkit.Latest(it.UpdatedAt, at)
	}
	it.UpdatedAt = modkit.Latest(it.UpdatedAt, it.CreatedAt)
	return it
}

// overlap is how far before the cursor a thread's last message still gets
// re-read: the list's times and the messages' differ by milliseconds, and
// equal times must never be skipped.
const overlap = 2 * time.Second

// SyncItems implements provider.Provider: a real delta — list pages (sorted
// by last message) until a thread's last message is older than since, each
// newer thread re-read; since zero, or the hourly sweep, reads all.
func (p *Provider) SyncItems(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	mod := project.ExternalID
	if mod == "" {
		return nil, errors.New("factorio: empty project id")
	}
	now := p.opts.Now()
	p.mu.Lock()
	full := since.IsZero() || now.Sub(p.full[mod]) >= FullEvery
	p.mu.Unlock()
	seen := map[string]bool{}
	var items []provider.Item
	done := false
	for page, last := 1, 1; page <= last && page <= maxPages && !done; page++ {
		rows, lp, err := p.listPage(ctx, mod, page)
		if err != nil {
			return nil, fmt.Errorf("discussion page %d: %w", page, err)
		}
		last = lp
		for _, r := range rows {
			if seen[r.ID] { // pages shifted by a new message: already read
				continue
			}
			seen[r.ID] = true
			p.mods.Store(r.ID, mod)
			if !full && parseTime(r.Last).Before(since.Add(-overlap)) {
				done = true
				break
			}
			it, ok, err := p.thread(ctx, mod, r)
			if err != nil {
				return nil, fmt.Errorf("thread %s: %w", r.ID, err)
			}
			if ok {
				items = append(items, it)
			}
		}
		if len(rows) == 0 {
			break
		}
	}
	if full {
		p.mu.Lock()
		p.full[mod] = now
		p.mu.Unlock()
	}
	slices.SortStableFunc(items, func(a, b provider.Item) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	return items, nil
}

// DetectChanges implements provider.Poller: page 1 of the list (thread ids,
// reply counts, last-message times) against the last fingerprint.
func (p *Provider) DetectChanges(ctx context.Context, project provider.Project, st *provider.PollState) (provider.Changes, error) {
	ch := provider.Changes{Requests: 1}
	rows, last, err := p.listPage(ctx, project.ExternalID, 1)
	if err != nil {
		return ch, err
	}
	parts := []string{fmt.Sprint(last)}
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s:%d:%s:%s", r.ID, r.Replies, r.Last, r.Category))
	}
	ch.Overflow = modkit.PageChanged(st, sigKey, modkit.Signature(parts...), p.opts.Now(), FullEvery)
	return ch, nil
}

// FetchChanged implements provider.Poller (the check never lists numbers).
func (p *Provider) FetchChanged(ctx context.Context, project provider.Project, _ []int) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, time.Time{})
}

// FullReconcile implements provider.Poller: the delta since the cursor (the
// hourly sweep reads everything).
func (p *Provider) FullReconcile(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, since)
}

// ── sign-in ──────────────────────────────────────────────────────

// SignInSpec is the Factorio «Подключить» for signin.New (probe = the
// signed-in username on a portal page).
func SignInSpec(hc *http.Client, log *slog.Logger) signin.Spec {
	p := New(Options{HTTP: hc})
	return signin.Spec{
		Platform: Platform, LoginURL: signInURL, Domains: []string{"factorio.com"}, Origins: Origins, Log: log,
		Probe: func(ctx context.Context, jar *websession.Jar) (string, error) { return p.whoAmI(ctx, jar) },
	}
}

func (p *Provider) whoAmI(ctx context.Context, jar *websession.Jar) (string, error) {
	body, err := p.get(ctx, "/", jar)
	if err != nil {
		return "", err
	}
	if u := signedInUser(body); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("%w: factorio: signed out", provider.ErrNotSignedIn)
}

// Login implements provider.Loginer.
func (p *Provider) Login(ctx context.Context) (provider.Login, error) {
	if p.opts.Session == nil {
		return provider.Login{}, errors.New("factorio: sign-in unavailable")
	}
	return p.opts.Session.Login(ctx)
}

// LoginStatus implements provider.Loginer (no network).
func (p *Provider) LoginStatus(context.Context) (provider.Login, error) {
	if p.opts.Session == nil {
		return provider.Login{}, nil
	}
	return p.opts.Session.Status(), nil
}

// Logout implements provider.Logouter.
func (p *Provider) Logout(ctx context.Context) error {
	if p.opts.Session == nil {
		return nil
	}
	return p.opts.Session.Logout(ctx)
}

// CancelLogin implements provider.LoginCanceller.
func (p *Provider) CancelLogin(context.Context) error {
	if p.opts.Session != nil {
		p.opts.Session.Cancel()
	}
	return nil
}
