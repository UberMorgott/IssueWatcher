package curseforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// Fetcher is the browser side of the native engine (*browser.Browser), used
// only for a write the plain preflight did not clear.
type Fetcher interface {
	Fetch(ctx context.Context, req browser.Request) (browser.Response, error)
}

// NativeOptions configures the native engine (docs/ARCHITECTURE.md → Native
// mod platforms): comments over the site's own JSON API (plain HTTP, no
// cookies), projects over CFWidget, the account and replies with the stored
// session.
type NativeOptions struct {
	Browser Fetcher
	Session *signin.Manager
	HTTP    *http.Client
	// Site and Widget override https://www.curseforge.com and
	// https://api.cfwidget.com (tests).
	Site, Widget string
}

// Origins are the https origins the native engine uses in the browser.
var Origins = []string{"https://www.curseforge.com"}

const (
	siteOrigin   = "https://www.curseforge.com"
	widgetOrigin = "https://api.cfwidget.com"
	signInURL    = "https://www.curseforge.com/login"
)

// SignInSpec is the CurseForge «Подключить» for signin.New (probe = the
// site's users/profile over the candidate cookies).
func SignInSpec(hc *http.Client, log *slog.Logger) signin.Spec {
	n := &native{opts: NativeOptions{HTTP: hc}, now: time.Now}
	n.fill()
	return signin.Spec{
		Platform: Platform, LoginURL: signInURL, Domains: []string{"curseforge.com"}, Origins: Origins, Log: log,
		Probe: func(ctx context.Context, jar *websession.Jar) (string, error) {
			s, err := n.profile(ctx, jar)
			if err != nil {
				return "", err
			}
			if !s.LoggedIn || s.name() == "" {
				return "", fmt.Errorf("%w: curseforge: %s", provider.ErrNotSignedIn, s.Detail)
			}
			return s.name(), nil
		},
	}
}

type native struct {
	opts NativeOptions
	log  *slog.Logger
	now  func() time.Time
}

func newNative(opts NativeOptions, log *slog.Logger, now func() time.Time) *native {
	n := &native{opts: opts, log: log, now: now}
	n.fill()
	return n
}

func (n *native) fill() {
	if n.opts.Site == "" {
		n.opts.Site = siteOrigin
	}
	if n.opts.Widget == "" {
		n.opts.Widget = widgetOrigin
	}
}

func (n *native) sigKey() string { return "v2:curseforge:page1" }
func (n *native) v2() bool       { return true }
func (n *native) scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: 5 * time.Minute, RateBudget: 120}
}

func host(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Hostname()
}

func (n *native) client(jar *websession.Jar) *websession.Client {
	return &websession.Client{Jar: jar, Hosts: []string{host(n.opts.Site), host(n.opts.Widget)}, HTTP: n.opts.HTTP}
}

func (n *native) jar() *websession.Jar {
	if n.opts.Session == nil {
		return nil
	}
	return n.opts.Session.Jar()
}

// getJSON reads u (anonymous unless jar) and decodes it.
func (n *native) getJSON(ctx context.Context, u string, jar *websession.Jar, out any) (int, error) {
	res, err := n.client(jar).Do(ctx, http.MethodGet, u, http.Header{"Accept": {"application/json"}}, nil, jar == nil)
	switch {
	case errors.Is(err, websession.ErrChallenge):
		return res.Status, fmt.Errorf("%w: %w", ErrRelogin, err)
	case err != nil:
		return 0, err
	case res.Status == http.StatusTooManyRequests:
		return res.Status, &provider.RateLimitError{Reset: n.now().Add(15 * time.Minute)}
	case res.Status < 200 || res.Status >= 300:
		return res.Status, fmt.Errorf("curseforge: HTTP %d: %s", res.Status, snippet(res.Body))
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return res.Status, fmt.Errorf("curseforge: decode %s: %w", u, err)
	}
	return res.Status, nil
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(tagRe.ReplaceAllString(string(b), " ")), " ")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// ── session ──────────────────────────────────────────────────────

// profile is the session check as the site does it: users/profile (200 +
// userId when signed in).
func (n *native) profile(ctx context.Context, jar *websession.Jar) (sessionResult, error) {
	s := sessionResult{CookiesStored: jar != nil && !jar.Empty()}
	if !s.CookiesStored {
		s.Detail = "no session cookies"
		return s, nil
	}
	res, err := n.client(jar).Do(ctx, http.MethodGet, n.opts.Site+"/api/v1/users/profile", http.Header{"Accept": {"application/json"}}, nil, false)
	switch {
	case errors.Is(err, websession.ErrChallenge):
		s.Detail = "Cloudflare challenge"
		return s, nil
	case err != nil:
		return s, err
	}
	return profileAnswer(s, res.Status, res.Body)
}

// profileAnswer reads a users/profile answer into s.
func profileAnswer(s sessionResult, status int, body []byte) (sessionResult, error) {
	var p struct {
		UserID      json.Number `json:"userId"`
		DisplayName *string     `json:"displayName"`
		UserName    *string     `json:"userName"`
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		s.Detail = fmt.Sprintf("HTTP %d (session expired)", status)
		return s, nil
	case status != http.StatusOK:
		return s, fmt.Errorf("curseforge: users/profile: HTTP %d", status)
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return s, fmt.Errorf("curseforge: users/profile: %w", err)
	}
	if p.UserID == "" || p.UserID == "0" {
		s.Detail = "profile has no user (signed out)"
		return s, nil
	}
	s.LoggedIn, s.Detail = true, "session valid"
	s.User = &struct {
		DisplayName *string `json:"displayName"`
		Username    *string `json:"username"`
	}{DisplayName: p.DisplayName, Username: p.UserName}
	return s, nil
}

func (n *native) status(ctx context.Context) (sessionResult, error) {
	s, _, err := n.check(ctx, n.jar())
	return s, err
}

// check is the session over plain HTTP, else (a challenged answer) inside
// the browser; plain tells which one answered.
func (n *native) check(ctx context.Context, jar *websession.Jar) (sessionResult, bool, error) {
	s, err := n.profile(ctx, jar)
	if err != nil || s.LoggedIn || s.Detail != "Cloudflare challenge" || n.opts.Browser == nil {
		return s, true, err
	}
	r, err := n.opts.Browser.Fetch(ctx, browser.Request{URL: n.opts.Site + "/api/v1/users/profile", Headers: map[string]string{"Accept": "application/json"}})
	if err != nil {
		return s, false, nil //nolint:nilerr // the plain answer (challenged) stands
	}
	bs, err := profileAnswer(s, r.Status, []byte(r.Body))
	return bs, false, err
}

func (n *native) login(ctx context.Context) (provider.Login, error) {
	if n.opts.Session == nil {
		return provider.Login{}, errors.New("curseforge: sign-in unavailable")
	}
	return n.opts.Session.Login(ctx)
}

func (n *native) loginStatus(context.Context) (provider.Login, error) {
	if n.opts.Session == nil {
		return provider.Login{}, nil
	}
	return n.opts.Session.Status(), nil
}

func (n *native) logout(ctx context.Context) error {
	if n.opts.Session == nil {
		return nil
	}
	return n.opts.Session.Logout(ctx)
}

func (n *native) cancelLogin(context.Context) error {
	if n.opts.Session != nil {
		n.opts.Session.Cancel()
	}
	return nil
}

// ── projects (CFWidget, keyless) ─────────────────────────────────

func (n *native) author(ctx context.Context, name string) (authorResult, error) {
	var d struct {
		Projects []struct {
			ID   json.Number `json:"id"`
			Name any         `json:"name"`
		} `json:"projects"`
	}
	if _, err := n.getJSON(ctx, n.opts.Widget+"/author/search/"+url.PathEscape(name), nil, &d); err != nil {
		return authorResult{}, fmt.Errorf("CFWidget: %w", err)
	}
	var r authorResult
	for _, p := range d.Projects {
		id, _ := strconv.Atoi(p.ID.String())
		name := ""
		if p.Name != nil {
			name = fmt.Sprint(p.Name)
		}
		r.Projects = append(r.Projects, struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}{ID: id, Name: name})
	}
	return r, nil
}

func (n *native) project(ctx context.Context, id int) (projectResult, error) {
	var d struct {
		Summary string `json:"summary"`
		URLs    struct {
			CurseForge *string `json:"curseforge"`
			Project    *string `json:"project"`
		} `json:"urls"`
	}
	if _, err := n.getJSON(ctx, fmt.Sprintf("%s/%d", n.opts.Widget, id), nil, &d); err != nil {
		return projectResult{}, fmt.Errorf("CFWidget: %w", err)
	}
	r := projectResult{Summary: d.Summary, URL: d.URLs.CurseForge}
	if r.URL == nil {
		r.URL = d.URLs.Project
	}
	return r, nil
}

// ── comments (site JSON, plain, no cookies) ─────────────────────

type apiComment struct {
	ID           json.Number     `json:"id"`
	ParentID     json.RawMessage `json:"parentId"`
	Text         any             `json:"text"`
	Body         *string         `json:"body"`
	RenderedHTML *string         `json:"renderedHtml"`
	Author       *struct {
		DisplayName string `json:"displayName"`
		Username    string `json:"username"`
	} `json:"author"`
	DatePosted   json.RawMessage `json:"datePosted"`
	DateModified json.RawMessage `json:"dateModified"`
	DateEdited   json.RawMessage `json:"dateEdited"`
	IsPinned     bool            `json:"isPinned"`
	Replies      []apiComment    `json:"replies"`
}

// base maps an entry as the reference server's commentsJson.
func (c apiComment) base() comment {
	out := comment{ID: c.ID.String(), Author: "?"}
	if c.Author != nil {
		switch {
		case c.Author.DisplayName != "":
			out.Author = c.Author.DisplayName
		case c.Author.Username != "":
			out.Author = c.Author.Username
		}
	}
	out.CreatedAt = isoUTC(c.DatePosted)
	mod := c.DateModified
	if isNull(mod) {
		mod = c.DateEdited
	}
	out.UpdatedAt = isoUTC(mod)
	if s, ok := c.Text.(string); ok && s != "" {
		out.Body = strings.ReplaceAll(s, "\r\n", "\n")
	} else {
		h := ""
		switch {
		case c.RenderedHTML != nil:
			h = *c.RenderedHTML
		case c.Body != nil:
			h = *c.Body
		}
		out.Body = StripHTML(h)
	}
	return out
}

func isNull(r json.RawMessage) bool {
	s := strings.TrimSpace(string(r))
	return s == "" || s == "null"
}

// isoUTC is the server's isoUtc: unix s|ms number or a date string → ISO UTC.
func isoUTC(r json.RawMessage) *string {
	if isNull(r) {
		return nil
	}
	var t time.Time
	var f float64
	var s string
	switch {
	case json.Unmarshal(r, &f) == nil:
		if f == 0 || math.IsNaN(f) {
			return nil
		}
		if f < 1e12 {
			f *= 1000
		}
		t = time.UnixMilli(int64(f))
	case json.Unmarshal(r, &s) == nil:
		if s == "" {
			return nil
		}
		p, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil
		}
		t = p
	default:
		return nil
	}
	v := t.UTC().Format("2006-01-02T15:04:05.000Z")
	return &v
}

func (n *native) page(ctx context.Context, mod, page int) (commentsResult, error) {
	var d struct {
		Data       []apiComment `json:"data"`
		Pagination *struct {
			TotalCount *int `json:"totalCount"`
			PageSize   int  `json:"pageSize"`
		} `json:"pagination"`
	}
	if _, err := n.getJSON(ctx, fmt.Sprintf("%s/api/v1/mods/%d/comments?page=%d", n.opts.Site, mod, page-1), nil, &d); err != nil {
		return commentsResult{}, err
	}
	r := commentsResult{Page: page, Comments: []thread{}}
	if d.Pagination != nil && d.Pagination.TotalCount != nil {
		size := d.Pagination.PageSize
		if size <= 0 {
			size = 20
		}
		total := *d.Pagination.TotalCount
		pages := max(1, (total+size-1)/size)
		r.Total, r.Pages = &total, &pages
	}
	var flat func([]apiComment) []comment
	flat = func(list []apiComment) []comment {
		var out []comment
		for _, c := range list {
			out = append(out, c.base())
			out = append(out, flat(c.Replies)...)
		}
		return out
	}
	for _, c := range d.Data {
		t := thread{comment: c.base(), Pinned: c.IsPinned, Replies: flat(c.Replies)}
		if t.Replies == nil {
			t.Replies = []comment{}
		}
		r.Comments = append(r.Comments, t)
	}
	return r, nil
}

var (
	brRe      = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockRe   = regexp.MustCompile(`(?i)</?(p|div|h[1-6]|li|tr|blockquote|pre|hr)[^>]*>`)
	anyTagRe  = regexp.MustCompile(`<[^>]*>`)
	decRe     = regexp.MustCompile(`&#(\d+);`)
	hexRe     = regexp.MustCompile(`&#x([0-9a-fA-F]+);`)
	blanksRe  = regexp.MustCompile(`[ \t]+`)
	spaceNLRe = regexp.MustCompile(` ?\n ?`)
	manyNLRe  = regexp.MustCompile(`\n{3,}`)
)

// StripHTML is the reference server's stripHtml (the fallback body when an
// entry has no plain text).
func StripHTML(h string) string {
	s := brRe.ReplaceAllString(h, "\n")
	s = blockRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")
	for _, r := range [][2]string{{"&amp;", "&"}, {"&lt;", "<"}, {"&gt;", ">"}, {"&quot;", `"`}, {"&#39;", "'"}, {"&nbsp;", " "}} {
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	s = decRe.ReplaceAllStringFunc(s, func(m string) string {
		v, _ := strconv.Atoi(decRe.FindStringSubmatch(m)[1])
		return string(rune(v & 0xFFFF))
	})
	s = hexRe.ReplaceAllStringFunc(s, func(m string) string {
		v, _ := strconv.ParseInt(hexRe.FindStringSubmatch(m)[1], 16, 64)
		return string(rune(v & 0xFFFF))
	})
	s = blanksRe.ReplaceAllString(s, " ")
	s = spaceNLRe.ReplaceAllString(s, "\n")
	s = manyNLRe.ReplaceAllString(s, "\n\n")
	return modkit.JSTrim(s)
}

// ── writes ───────────────────────────────────────────────────────

// post sends one comment. A read-only preflight (users/profile over plain
// HTTP) picks the transport: plain when it answers 200, else the browser.
// Anything but a clear answer is errWriteUnsure (read back, never re-sent).
func (n *native) post(ctx context.Context, mod int, html string, parent int) (postResult, error) {
	jar := n.jar()
	if jar == nil || jar.Empty() {
		return postResult{}, fmt.Errorf("%w: curseforge: no session (Settings › Платформы › Подключить)", provider.ErrNotSignedIn)
	}
	s, plain, err := n.check(ctx, jar)
	if err != nil {
		return postResult{}, err
	}
	if !s.LoggedIn {
		return postResult{}, fmt.Errorf("%w (%s)", ErrRelogin, s.Detail)
	}
	payload := map[string]any{"entityId": mod, "body": html, "bodyType": "RawHtml"}
	if parent > 0 {
		payload["parentId"] = parent
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return postResult{}, err
	}
	xsrf := jar.Value("XSRF-TOKEN")
	if xsrf == "" {
		xsrf = jar.Value("X-XSRF-TOKEN")
	}
	var status int
	var answer []byte
	if plain {
		h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
		if xsrf != "" {
			h.Set("X-XSRF-TOKEN", xsrf)
		}
		res, err := n.client(jar).Do(ctx, http.MethodPost, n.opts.Site+"/api/v1/comments", h, body, false)
		if err != nil { // lost answer, timeout or a challenge: it may have been saved
			return postResult{}, fmt.Errorf("%w: POST /api/v1/comments: %w", errWriteUnsure, err)
		}
		status, answer = res.Status, res.Body
	} else {
		hdr := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}
		if xsrf != "" {
			hdr["X-XSRF-TOKEN"] = xsrf
		}
		r, err := n.opts.Browser.Fetch(ctx, browser.Request{URL: n.opts.Site + "/api/v1/comments", Method: http.MethodPost, Headers: hdr, Body: string(body)})
		if err != nil {
			return postResult{}, fmt.Errorf("%w: POST /api/v1/comments (browser): %w", errWriteUnsure, err)
		}
		status, answer = r.Status, []byte(r.Body)
	}
	switch {
	case status >= 200 && status < 300:
		var j struct {
			ID   json.Number `json:"id"`
			Data *struct {
				ID json.Number `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(answer, &j)
		id := j.ID.String()
		if id == "" && j.Data != nil {
			id = j.Data.ID.String()
		}
		if id == "" {
			return postResult{Posted: true}, nil // read back by the caller
		}
		return postResult{Posted: true, ID: &id}, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return postResult{}, fmt.Errorf("%w: HTTP %d", ErrRelogin, status)
	case status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests:
		return postResult{}, fmt.Errorf("curseforge: comment refused: HTTP %d: %s", status, snippet(answer))
	}
	return postResult{}, fmt.Errorf("%w: HTTP %d: %s", errWriteUnsure, status, snippet(answer))
}
