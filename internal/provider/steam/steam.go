// Package steam is the native Steam Workshop provider (docs/ARCHITECTURE.md →
// Mod platforms): the owner's Workshop items are projects, every root comment
// is an item of kind comment. Reads are keyless (community pages and the
// comment render endpoint); a Web API key, when set, lists the items through
// IPublishedFileService/GetUserFiles instead of the profile page.
package steam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Platform is the provider's platform name (sources.platform, settings keys).
const Platform = "steam"

const (
	defaultCommunity = "https://steamcommunity.com"
	defaultAPI       = "https://api.steampowered.com"
	defaultLogin     = "https://login.steampowered.com"
	pageSize         = 50 // comments per render request (Steam honours 50)
	filesPerPage     = 30 // myworkshopfiles numperpage
	maxPages         = 200
	userAgent        = "IssueWatcher (+https://github.com/UberMorgott/issuewatcher)"
)

// Options configures a Provider.
type Options struct {
	Dir          string // data\secrets (steam.json)
	CommunityURL string // default https://steamcommunity.com (tests: httptest)
	APIURL       string // default https://api.steampowered.com
	LoginURL     string // default https://login.steampowered.com
	HTTP         *http.Client
	Log          *slog.Logger
	Now          func() time.Time
	// MinGap is the pause between two requests (politeness; default 1 s).
	MinGap time.Duration
	// PageSize is the comments per render request (default 50).
	PageSize int
	// QRInterval overrides the QR poll interval Steam asks for (tests).
	QRInterval time.Duration
}

// Provider reads (and, with the user's cookies, posts) Workshop comments.
type Provider struct {
	opts Options

	save     sync.Mutex // serialises settings writes (Save, the session flag): read-apply-write-swap is one step
	mu       sync.Mutex
	settings Settings
	loaded   bool
	creators map[string]string // publishedfileid → creator SteamID64

	gap  sync.Mutex // serialises requests: one at a time, MinGap apart
	last time.Time

	qmu      sync.Mutex // the QR sign-in
	qr       *qrLogin
	onSignIn func()
}

// New creates the provider; settings are read lazily from Options.Dir.
func New(opts Options) *Provider {
	if opts.CommunityURL == "" {
		opts.CommunityURL = defaultCommunity
	}
	if opts.APIURL == "" {
		opts.APIURL = defaultAPI
	}
	if opts.LoginURL == "" {
		opts.LoginURL = defaultLogin
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MinGap == 0 {
		opts.MinGap = time.Second
	}
	if opts.PageSize <= 0 {
		opts.PageSize = pageSize
	}
	opts.CommunityURL = strings.TrimRight(opts.CommunityURL, "/")
	opts.APIURL = strings.TrimRight(opts.APIURL, "/")
	opts.LoginURL = strings.TrimRight(opts.LoginURL, "/")
	return &Provider{opts: opts, creators: map[string]string{}}
}

// Platform implements provider.Provider.
func (p *Provider) Platform() string { return Platform }

// Capabilities implements provider.Provider. Reply is on only while the
// stored web session has passed a check (QR sign-in, «Проверить» or a post)
// and Steam has not refused it since.
func (p *Provider) Capabilities() provider.Capabilities {
	s, _ := p.current()
	return provider.Capabilities{
		ListProjects: true, SyncItems: true, ListComments: true,
		Reply: s.LoginSecure != "" && s.SessionID != "" && s.Verified && !s.SessionExpired,
		Auth:  provider.AuthCookieSession, Kinds: []string{"comment"},
	}
}

// Scheduling implements provider.Poller: unofficial endpoints, stay modest.
func (p *Provider) Scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: 5 * time.Minute, RateBudget: 120}
}

func (p *Provider) current() (Settings, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded {
		s, err := loadSettings(p.opts.Dir)
		if err != nil {
			return Settings{}, err
		}
		p.settings, p.loaded = s, true
	}
	return p.settings, nil
}

// Status is the non-secret settings view.
func (p *Provider) Status() (Status, error) {
	s, err := p.current()
	return s.status(), err
}

// Save applies u and stores the settings (DPAPI). A changed SteamID drops the
// creator cache.
func (p *Provider) Save(u Update) (Status, error) {
	p.save.Lock()
	defer p.save.Unlock()
	cur, err := p.current()
	if err != nil {
		return Status{}, err
	}
	next, err := cur.apply(u)
	if err != nil {
		return Status{}, err
	}
	if err := saveSettings(p.opts.Dir, next); err != nil {
		return Status{}, err
	}
	p.mu.Lock()
	p.settings = next
	if next.SteamID != cur.SteamID {
		p.creators = map[string]string{}
	}
	p.mu.Unlock()
	return next.status(), nil
}

// Account implements provider.Provider: the SteamID64, ErrNotSignedIn until set.
func (p *Provider) Account(context.Context) (string, error) {
	s, err := p.current()
	if err != nil {
		return "", err
	}
	if s.SteamID == "" {
		return "", provider.ErrNotSignedIn
	}
	return s.SteamID, nil
}

// fileURL is a Workshop item's page.
func (p *Provider) fileURL(id string) string {
	return p.opts.CommunityURL + "/sharedfiles/filedetails/?id=" + id
}

// answer is a read response: status, headers, final URL (after redirects), body.
type answer struct {
	Status int
	Header http.Header
	URL    *url.URL
	Body   []byte
}

// do sends one request, MinGap after the previous one; 429/503 → RateLimitError.
func (p *Provider) do(req *http.Request) (answer, error) {
	p.gap.Lock()
	if wait := p.opts.MinGap - p.opts.Now().Sub(p.last); wait > 0 && !p.last.IsZero() {
		t := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			t.Stop()
			p.gap.Unlock()
			return answer{}, req.Context().Err()
		case <-t.C:
		}
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.opts.HTTP.Do(req)
	p.last = p.opts.Now()
	p.gap.Unlock()
	if err != nil {
		return answer{}, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	_ = resp.Body.Close()
	a := answer{Status: resp.StatusCode, Header: resp.Header, URL: resp.Request.URL, Body: body}
	if err != nil {
		return a, fmt.Errorf("steam: read %s: %w", req.URL.Path, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		rl := &provider.RateLimitError{Secondary: true} // syncer backs off exponentially
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			rl.Reset = p.opts.Now().Add(time.Duration(s) * time.Second)
		}
		return a, rl
	}
	return a, nil
}

func (p *Provider) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	a, err := p.do(req)
	if err != nil {
		return nil, err
	}
	if a.Status != http.StatusOK {
		return nil, fmt.Errorf("steam: GET %s: HTTP %d", req.URL.Path, a.Status)
	}
	return a.Body, nil
}

func (p *Provider) postForm(ctx context.Context, u string, form url.Values, cookie string) (answer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return p.do(req)
}

// ListProjects implements provider.Provider: the owner's Workshop items.
func (p *Provider) ListProjects(ctx context.Context) ([]provider.Project, error) {
	s, err := p.current()
	if err != nil {
		return nil, err
	}
	if s.SteamID == "" {
		return nil, provider.ErrNotSignedIn
	}
	var files []workshopFile
	if s.APIKey != "" {
		files, err = p.userFiles(ctx, s)
		if err != nil {
			p.opts.Log.Warn("steam: GetUserFiles failed, reading the profile page", "err", err)
		}
	}
	if s.APIKey == "" || err != nil {
		files, err = p.profileFiles(ctx, s)
		if err != nil {
			return nil, err
		}
	}
	out := make([]provider.Project, 0, len(files))
	for _, f := range files {
		name := f.Title
		if name == "" {
			name = f.ID
		}
		out = append(out, provider.Project{ExternalID: f.ID, Name: name, URL: p.fileURL(f.ID)})
	}
	return out, nil
}

// profileFiles scrapes steamcommunity.com/profiles/<id>/myworkshopfiles (keyless).
func (p *Provider) profileFiles(ctx context.Context, s Settings) ([]workshopFile, error) {
	var (
		out  []workshopFile
		seen = map[string]bool{}
	)
	for page := 1; page <= maxPages; page++ {
		q := url.Values{"p": {strconv.Itoa(page)}, "numperpage": {strconv.Itoa(filesPerPage)}, "l": {"english"}}
		if s.AppID > 0 {
			q.Set("appid", strconv.Itoa(s.AppID))
		}
		body, err := p.get(ctx, p.opts.CommunityURL+"/profiles/"+s.SteamID+"/myworkshopfiles/?"+q.Encode())
		if err != nil {
			return nil, err
		}
		files, total, err := parseWorkshopFiles(string(body))
		if err != nil {
			return nil, err
		}
		if total < 0 {
			// Neither items nor the counter: private profile or changed markup.
			// An error keeps the known projects active (never an empty list).
			return nil, errors.New("steam: workshop page not recognised (private profile?)")
		}
		added := 0
		for _, f := range files {
			if !seen[f.ID] {
				seen[f.ID] = true
				out = append(out, f)
				added++
			}
		}
		if added == 0 || len(out) >= total {
			break
		}
	}
	if err := p.fillCreators(ctx, out); err != nil {
		p.opts.Log.Warn("steam: creator lookup failed", "err", err)
	}
	return out, nil
}

// userFiles lists the owner's items with the Web API key.
func (p *Provider) userFiles(ctx context.Context, s Settings) ([]workshopFile, error) {
	var out []workshopFile
	for page := 1; page <= maxPages; page++ {
		q := url.Values{
			"key": {s.APIKey}, "steamid": {s.SteamID}, "appid": {strconv.Itoa(s.AppID)},
			"page": {strconv.Itoa(page)}, "numperpage": {"100"}, "type": {"myfiles"},
		}
		body, err := p.get(ctx, p.opts.APIURL+"/IPublishedFileService/GetUserFiles/v1/?"+q.Encode())
		if err != nil {
			return nil, errors.New(strings.ReplaceAll(err.Error(), s.APIKey, "***")) // never log the key
		}
		var r struct {
			Response struct {
				Total int `json:"total"`
				Files []struct {
					ID      string `json:"publishedfileid"`
					Creator string `json:"creator"`
					AppID   int    `json:"consumer_appid"`
					Title   string `json:"title"`
				} `json:"publishedfiledetails"`
			} `json:"response"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("steam: GetUserFiles: %w", err)
		}
		p.mu.Lock()
		for _, f := range r.Response.Files {
			out = append(out, workshopFile{ID: f.ID, AppID: f.AppID, Title: f.Title})
			if f.Creator != "" {
				p.creators[f.ID] = f.Creator
			}
		}
		p.mu.Unlock()
		if len(r.Response.Files) == 0 || len(out) >= r.Response.Total {
			break
		}
	}
	return out, nil
}

// fillCreators looks up the creators the cache lacks (one keyless
// GetPublishedFileDetails request for all of them).
func (p *Provider) fillCreators(ctx context.Context, files []workshopFile) error {
	form := url.Values{}
	p.mu.Lock()
	for _, f := range files {
		if _, ok := p.creators[f.ID]; !ok {
			form.Set("publishedfileids["+strconv.Itoa(len(form))+"]", f.ID)
		}
	}
	p.mu.Unlock()
	if len(form) == 0 {
		return nil
	}
	form.Set("itemcount", strconv.Itoa(len(form)))
	a, err := p.postForm(ctx, p.opts.APIURL+"/ISteamRemoteStorage/GetPublishedFileDetails/v1/", form, "")
	if err != nil {
		return err
	}
	if a.Status != http.StatusOK {
		return fmt.Errorf("steam: GetPublishedFileDetails: HTTP %d", a.Status)
	}
	body := a.Body
	var r struct {
		Response struct {
			Details []struct {
				ID      string `json:"publishedfileid"`
				Result  int    `json:"result"`
				Creator string `json:"creator"`
			} `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("steam: GetPublishedFileDetails: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range r.Response.Details {
		if d.Result == 1 && d.Creator != "" {
			p.creators[d.ID] = d.Creator
		}
	}
	return nil
}

// creator returns the item's creator (the render URL needs it).
func (p *Provider) creator(ctx context.Context, fileID string) (string, error) {
	p.mu.Lock()
	c, ok := p.creators[fileID]
	p.mu.Unlock()
	if ok {
		return c, nil
	}
	if err := p.fillCreators(ctx, []workshopFile{{ID: fileID}}); err != nil {
		return "", err
	}
	p.mu.Lock()
	c, ok = p.creators[fileID]
	p.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("steam: item %s not found (deleted or private?)", fileID)
	}
	return c, nil
}

// renderPage is one answer of the comment render endpoint.
type renderPage struct {
	Success      bool   `json:"success"`
	TotalCount   int    `json:"total_count"`
	TimeLastPost int64  `json:"timelastpost"`
	HTML         string `json:"comments_html"`
	Error        string `json:"error"`
}

func (p *Provider) threadPath(creator, fileID string) string {
	return "/comment/PublishedFile_Public/%s/" + creator + "/" + fileID + "/"
}

// render reads count comments from start (newest first).
func (p *Provider) render(ctx context.Context, creator, fileID string, start, count int) (renderPage, error) {
	u := p.opts.CommunityURL + fmt.Sprintf(p.threadPath(creator, fileID), "render")
	form := url.Values{"start": {strconv.Itoa(start)}, "count": {strconv.Itoa(count)}}
	a, err := p.postForm(ctx, u, form, "")
	if err != nil {
		return renderPage{}, err
	}
	if a.Status != http.StatusOK {
		return renderPage{}, fmt.Errorf("steam: comments of %s: HTTP %d", fileID, a.Status)
	}
	var r renderPage
	if err := json.Unmarshal(a.Body, &r); err != nil {
		return renderPage{}, fmt.Errorf("steam: comments of %s: %w", fileID, err)
	}
	if !r.Success {
		return renderPage{}, fmt.Errorf("steam: comments of %s: %s", fileID, cmpStr(r.Error, "success=false"))
	}
	return r, nil
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// validator is the cheap change marker of a thread (DetectChanges).
func validator(r renderPage) string {
	return strconv.FormatInt(r.TimeLastPost, 10) + "/" + strconv.Itoa(r.TotalCount)
}

// SyncItems implements provider.Provider: one item per comment created at or
// after since (zero = all), oldest first. Pages newest → oldest and stops at
// the first page reaching past since (that page is the overlap).
func (p *Provider) SyncItems(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	items, _, err := p.readThread(ctx, project, since)
	return items, err
}

func (p *Provider) readThread(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, renderPage, error) {
	s, err := p.current()
	if err != nil {
		return nil, renderPage{}, err
	}
	creator, err := p.creator(ctx, project.ExternalID)
	if err != nil {
		return nil, renderPage{}, err
	}
	var (
		all   []comment
		first renderPage
		seen  = map[string]bool{}
	)
	for start, page := 0, 0; page < maxPages; page++ {
		r, err := p.render(ctx, creator, project.ExternalID, start, p.opts.PageSize)
		if err != nil {
			return nil, renderPage{}, err // incomplete: the caller keeps its cursor
		}
		if page == 0 {
			first = r
		}
		cs, err := parseComments(r.HTML)
		if err != nil {
			return nil, renderPage{}, err
		}
		older := false
		for _, c := range cs {
			if !seen[c.ID] {
				seen[c.ID] = true
				all = append(all, c)
			}
			if !since.IsZero() && c.CreatedAt.Before(since) {
				older = true
			}
		}
		start += len(cs)
		if len(cs) == 0 || older || start >= r.TotalCount {
			break
		}
	}
	items := make([]provider.Item, 0, len(all))
	for _, c := range slices.Backward(all) { // oldest first
		if !since.IsZero() && c.CreatedAt.Before(since) {
			continue
		}
		items = append(items, p.item(project, c, itemNumber(c.ID), s.SteamID))
	}
	return items, first, nil
}

// itemNumber is a comment's stable item number: the low 31 bits of its id (a
// position in the thread would repeat after a deletion; the full id does not
// fit a JavaScript number). 0 → 1.
func itemNumber(commentID string) int {
	n, err := strconv.ParseUint(commentID, 10, 64)
	if err != nil {
		return 0
	}
	return max(int(n&0x7fffffff), 1)
}

// ItemExternalID is a comment item's key: comment:<publishedfileid>/<commentid>
// (the file id is needed to reply; comment ids alone are unique too).
func ItemExternalID(fileID, commentID string) string {
	return "comment:" + fileID + "/" + commentID
}

// item maps a comment. The owner's own comments carry the account (SteamID64)
// as author, so the store's own-author filter recognises them.
func (p *Provider) item(project provider.Project, c comment, number int, self string) provider.Item {
	author := c.Author
	if c.SteamID != "" && c.SteamID == self {
		author = self
	}
	return provider.Item{
		ExternalID: ItemExternalID(project.ExternalID, c.ID),
		Kind:       "comment",
		Number:     number,
		Title:      title(c.Body),
		Body:       c.Body,
		URL:        p.fileURL(project.ExternalID) + "#comment_" + c.ID,
		Author:     author,
		Open:       true,
		CreatedAt:  c.CreatedAt,
		UpdatedAt:  c.CreatedAt,
	}
}

// title is the first line of the body, at most 80 characters.
func title(body string) string {
	line, _, _ := strings.Cut(body, "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 80 {
		line = string(r[:79]) + "…"
	}
	if line == "" {
		line = "(comment)"
	}
	return line
}

// DetectChanges implements provider.Poller: one render request of a single
// comment; timelastpost + total_count against the stored validator.
func (p *Provider) DetectChanges(ctx context.Context, project provider.Project, st *provider.PollState) (provider.Changes, error) {
	creator, err := p.creator(ctx, project.ExternalID)
	if err != nil {
		return provider.Changes{}, err
	}
	r, err := p.render(ctx, creator, project.ExternalID, 0, 1)
	if err != nil {
		return provider.Changes{Requests: 1}, err
	}
	key := fmt.Sprintf(p.threadPath(creator, project.ExternalID), "render")
	if st.ETags == nil {
		st.ETags = map[string]string{}
	}
	v := validator(r)
	if st.ETags[key] == v {
		return provider.Changes{Requests: 1, NotModified: 1}, nil
	}
	st.ETags[key] = v
	return provider.Changes{Requests: 1, Overflow: true}, nil // re-read the thread since the cursor
}

// FetchChanged implements provider.Poller; DetectChanges never lists numbers.
func (p *Provider) FetchChanged(context.Context, provider.Project, []int) ([]provider.Item, error) {
	return nil, nil
}

// FullReconcile implements provider.Poller.
func (p *Provider) FullReconcile(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	return p.SyncItems(ctx, project, since)
}
