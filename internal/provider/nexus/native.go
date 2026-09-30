package nexus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// Fetcher is the browser side of the native engine (*browser.Browser).
type Fetcher interface {
	Fetch(ctx context.Context, req browser.Request) (browser.Response, error)
}

// NativeOptions configures the native engine (docs/ARCHITECTURE.md → Native
// mod platforms): projects over the keyless v2 GraphQL, the account over the
// site GraphQL with the stored session, comments and bug reports through
// in-page fetches of the installed browser (www is Cloudflare-challenged for
// plain HTTP).
type NativeOptions struct {
	Browser Fetcher
	// Session is the stored sign-in (nil = public reads only).
	Session *signin.Manager
	// HTTP carries the plain GraphQL requests (default http.DefaultClient).
	HTTP *http.Client
	// Site, GraphQL, APIRouter override the endpoints (tests).
	Site, GraphQL, APIRouter string
}

// Origins are the https origins the native engine uses in the browser.
var Origins = []string{"https://www.nexusmods.com"}

// SignInOrigins are the origins of the sign-in window (besides Origins).
var SignInOrigins = []string{"https://users.nexusmods.com", "https://next.nexusmods.com"}

// Endpoints of the native engine.
const (
	siteOrigin  = "https://www.nexusmods.com"
	graphqlURL  = "https://api.nexusmods.com/v2/graphql"
	apiRouter   = "https://api-router.nexusmods.com/graphql"
	signInURL   = "https://users.nexusmods.com/auth/sign_in"
	nativeSigV2 = "v2:nexus:page1"
)

// SignInSpec is the Nexus «Подключить» for signin.New (probe = the site
// GraphQL account over the candidate cookies).
func SignInSpec(hc *http.Client, log *slog.Logger) signin.Spec {
	n := &native{opts: NativeOptions{HTTP: hc}}
	n.fill()
	return signin.Spec{
		Platform: Platform, LoginURL: signInURL,
		Domains: []string{"nexusmods.com"}, Origins: append([]string{siteOrigin}, SignInOrigins...),
		Log: log,
		Probe: func(ctx context.Context, jar *websession.Jar) (string, error) {
			m, err := n.whoAmI(ctx, jar)
			if err != nil {
				return "", err
			}
			return m.name, nil
		},
	}
}

type native struct {
	opts NativeOptions
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	games   map[string]int // domain → game id
	threads map[string]int // game/mod → Posts thread id
}

func newNative(opts NativeOptions, log *slog.Logger, now func() time.Time) *native {
	n := &native{opts: opts, log: log, now: now, games: map[string]int{}, threads: map[string]int{}}
	n.fill()
	return n
}

func (n *native) fill() {
	if n.opts.Site == "" {
		n.opts.Site = siteOrigin
	}
	if n.opts.GraphQL == "" {
		n.opts.GraphQL = graphqlURL
	}
	if n.opts.APIRouter == "" {
		n.opts.APIRouter = apiRouter
	}
	if n.games == nil {
		n.games, n.threads = map[string]int{}, map[string]int{}
	}
}

func (n *native) sigKey() string { return nativeSigV2 }

func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Hostname()
}

// client is plain HTTP to the GraphQL hosts (jar nil = anonymous).
func (n *native) client(jar *websession.Jar) *websession.Client {
	return &websession.Client{Jar: jar, Hosts: []string{hostOf(n.opts.GraphQL), hostOf(n.opts.APIRouter)}, HTTP: n.opts.HTTP}
}

type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// graphql posts a query and decodes data into out.
func (n *native) graphql(ctx context.Context, endpoint string, jar *websession.Jar, op, query string, vars map[string]any, out any) error {
	req := map[string]any{"query": query, "variables": vars}
	if op != "" {
		req["operationName"] = op
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	if op != "" {
		h.Set("X-GraphQL-OperationName", op)
	}
	res, err := n.client(jar).Do(ctx, http.MethodPost, endpoint, h, body, jar == nil)
	if err != nil {
		return fmt.Errorf("nexus: graphql %s: %w", op, err)
	}
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.Unmarshal(res.Body, &r); err != nil {
		return fmt.Errorf("nexus: graphql %s: HTTP %d: %s", op, res.Status, snippet(res.Body))
	}
	if len(r.Errors) > 0 {
		msgs := make([]string, 0, len(r.Errors))
		unauth := false
		for _, e := range r.Errors {
			msgs = append(msgs, e.Message)
			unauth = unauth || e.Extensions.Code == "UNAUTHORIZED"
		}
		if unauth {
			return fmt.Errorf("%w: nexus: %s", provider.ErrNotSignedIn, strings.Join(msgs, "; "))
		}
		return fmt.Errorf("nexus: graphql %s: %s", op, strings.Join(msgs, "; "))
	}
	if res.Status == http.StatusTooManyRequests {
		return &provider.RateLimitError{Reset: n.now().Add(15 * time.Minute)}
	}
	if out != nil {
		return json.Unmarshal(r.Data, out)
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(string(b), " ")), " ")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// ── projects ─────────────────────────────────────────────────────

const modsQuery = `query($f:ModsFilter,$s:[ModsSort!],$c:Int,$o:Int){ mods(filter:$f, sort:$s, count:$c, offset:$o){ totalCount nodes{ modId name description uploader{ name memberId } game{ domainName } } } }`

func (n *native) searchMods(ctx context.Context, u uploader, count, offset int) (modsResult, error) {
	filter := map[string]any{}
	if u.id > 0 {
		filter["uploaderId"] = []map[string]any{{"value": strconv.Itoa(u.id), "op": "EQUALS"}}
	} else {
		filter["uploader"] = []map[string]any{{"value": u.name, "op": "EQUALS"}}
	}
	var d struct {
		Mods struct {
			TotalCount int `json:"totalCount"`
			Nodes      []struct {
				ModID       int    `json:"modId"`
				Name        string `json:"name"`
				Description string `json:"description"`
				Uploader    struct {
					Name     *string `json:"name"`
					MemberID *int    `json:"memberId"`
				} `json:"uploader"`
				Game struct {
					DomainName string `json:"domainName"`
				} `json:"game"`
			} `json:"nodes"`
		} `json:"mods"`
	}
	vars := map[string]any{"f": filter, "s": []map[string]any{{"relevance": map[string]string{"direction": "DESC"}}}, "c": count, "o": offset}
	if err := n.graphql(ctx, n.opts.GraphQL, nil, "", modsQuery, vars, &d); err != nil {
		return modsResult{}, err
	}
	r := modsResult{Total: d.Mods.TotalCount}
	for _, m := range d.Mods.Nodes {
		x := modRow{Game: m.Game.DomainName, ModID: m.ModID, Name: m.Name, URL: fmt.Sprintf("%s/%s/mods/%d", siteOrigin, m.Game.DomainName, m.ModID)}
		x.Uploader.Name, x.Uploader.MemberID = m.Uploader.Name, m.Uploader.MemberID
		x.codeURL = provider.GitHubRepoURL(m.Description)
		r.Mods = append(r.Mods, x)
	}
	return r, nil
}

func (n *native) gameID(ctx context.Context, domain string) (int, error) {
	n.mu.Lock()
	id, ok := n.games[domain]
	n.mu.Unlock()
	if ok {
		return id, nil
	}
	var d struct {
		Game *struct {
			ID int `json:"id"`
		} `json:"game"`
	}
	if err := n.graphql(ctx, n.opts.GraphQL, nil, "", `query($d:String){ game(domainName:$d){ id } }`, map[string]any{"d": domain}, &d); err != nil {
		return 0, err
	}
	if d.Game == nil || d.Game.ID == 0 {
		return 0, fmt.Errorf("nexus: unknown game domain %q", domain)
	}
	n.mu.Lock()
	n.games[domain] = d.Game.ID
	n.mu.Unlock()
	return d.Game.ID, nil
}

// ── site (browser) ───────────────────────────────────────────────

// page fetches a www URL in the browser as the site's own XHR does; a
// non-2xx answer is an error (read-only callers).
func (n *native) page(ctx context.Context, u, modPage string, form url.Values) (string, error) {
	if n.opts.Browser == nil {
		return "", errors.New("nexus: no browser")
	}
	req := browser.Request{URL: u, Page: modPage, Headers: map[string]string{"X-Requested-With": "XMLHttpRequest"}}
	if form != nil {
		req.Method = http.MethodPost
		req.Headers["Content-Type"] = "application/x-www-form-urlencoded; charset=UTF-8"
		req.Body = form.Encode()
	}
	r, err := n.opts.Browser.Fetch(ctx, req)
	if err != nil {
		return "", err
	}
	switch {
	case r.Status == http.StatusTooManyRequests:
		return "", &provider.RateLimitError{Reset: n.now().Add(15 * time.Minute)}
	case r.Status == http.StatusNotFound:
		return "", fmt.Errorf("%w: HTTP 404", errNotFound)
	case r.Status < 200 || r.Status >= 300:
		return "", fmt.Errorf("nexus: HTTP %d: %s", r.Status, snippet([]byte(r.Body)))
	}
	return r.Body, nil
}

func (n *native) modPage(game string, mod int) string {
	return fmt.Sprintf("%s/%s/mods/%d", n.opts.Site, game, mod)
}

func (n *native) threadID(ctx context.Context, game string, mod int) (int, error) {
	key := fmt.Sprintf("%s/%d", game, mod)
	n.mu.Lock()
	t, ok := n.threads[key]
	n.mu.Unlock()
	if ok {
		return t, nil
	}
	body, err := n.page(ctx, n.modPage(game, mod), n.modPage(game, mod), nil)
	if err != nil {
		return 0, err
	}
	t = parseThreadID(body)
	if t == 0 {
		return 0, fmt.Errorf("%w: no Posts thread found for %s (comments disabled, or mod hidden)", errDisabled, key)
	}
	n.mu.Lock()
	n.threads[key] = t
	n.mu.Unlock()
	return t, nil
}

func (n *native) commentPage(ctx context.Context, game string, mod, page int) (commentPage, int, int, error) {
	gid, err := n.gameID(ctx, game)
	if err != nil {
		return commentPage{}, 0, 0, err
	}
	tid, err := n.threadID(ctx, game, mod)
	if err != nil {
		return commentPage{}, 0, 0, err
	}
	u := fmt.Sprintf("%s/Core/Libs/Common/Widgets/CommentContainer?RH_CommentContainer=game_id:%d,object_id:%d,object_type:1,thread_id:%d,tabbed:1,skip_opening_post:0,page:%d",
		n.opts.Site, gid, mod, tid, page)
	body, err := n.page(ctx, u, n.modPage(game, mod), nil)
	if err != nil {
		return commentPage{}, 0, 0, err
	}
	cp, err := parseComments(body)
	return cp, gid, tid, err
}

func (n *native) comments(ctx context.Context, game string, mod, page int) (commentsResult, error) {
	cp, _, _, err := n.commentPage(ctx, game, mod, page)
	return cp.commentsResult, err
}

func (n *native) bugs(ctx context.Context, game string, mod, page int) (bugsResult, error) {
	gid, err := n.gameID(ctx, game)
	if err != nil {
		return bugsResult{}, err
	}
	u := fmt.Sprintf("%s/Core/Libs/Common/Widgets/ModBugsTab?RH_ModBugsTab=game_id:%d,id:%d,page_size:10,page:%d", n.opts.Site, gid, mod, page)
	body, err := n.page(ctx, u, n.modPage(game, mod), nil)
	if err != nil {
		return bugsResult{}, err
	}
	r, enabled, err := parseBugs(body, fmt.Sprintf("%s/%s/mods/%d?tab=bugs", siteOrigin, game, mod), page)
	if err != nil {
		return bugsResult{}, err
	}
	if !enabled {
		return bugsResult{}, fmt.Errorf("%w: bug reports are not available for %s/%d", errDisabled, game, mod)
	}
	return r, nil
}

func (n *native) bugReplies(ctx context.Context, issue int) (bugPage, error) {
	body, err := n.page(ctx, n.opts.Site+"/Core/Libs/Common/Widgets/ModBugReplyList", "", url.Values{"issue_id": {strconv.Itoa(issue)}})
	if err != nil {
		return bugPage{}, err
	}
	return parseBugReplies(body)
}

func (n *native) bug(ctx context.Context, issue int) (bugResult, error) {
	bp, err := n.bugReplies(ctx, issue)
	if err != nil {
		return bugResult{}, err
	}
	if len(bp.posts) == 0 || bp.posts[0].ID == "" {
		return bugResult{}, fmt.Errorf("%w: bug report %d not found (deleted) or not visible", errNotFound, issue)
	}
	return bp.result(), nil
}

// ── writes ───────────────────────────────────────────────────────

// submit sends one write in the browser; anything but a clear answer is
// errWriteUnsure (the caller reads back, never re-sends).
func (n *native) submit(ctx context.Context, path, modPage string, form url.Values) (browser.Response, error) {
	if n.opts.Browser == nil {
		return browser.Response{}, errors.New("nexus: no browser")
	}
	r, err := n.opts.Browser.Fetch(ctx, browser.Request{URL: n.opts.Site + path, Page: modPage, Method: http.MethodPost,
		Headers: map[string]string{"X-Requested-With": "XMLHttpRequest", "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8"},
		Body:    form.Encode()})
	if err != nil {
		return r, fmt.Errorf("%w: POST %s: %w", errWriteUnsure, path, err)
	}
	return r, nil
}

// refused reports a 4xx answer (nothing saved), 408 excepted.
func refused(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusRequestTimeout
}

func (n *native) signedIn() error {
	if n.opts.Session == nil || n.opts.Session.Jar().Empty() {
		return fmt.Errorf("%w: nexus: not signed in (Settings › Платформы › Подключить)", provider.ErrNotSignedIn)
	}
	return nil
}

func (n *native) postComment(ctx context.Context, game string, mod int, text string, parent int) (postResult, error) {
	if err := n.signedIn(); err != nil {
		return postResult{}, err
	}
	cp, gid, tid, err := n.commentPage(ctx, game, mod, 1)
	if err != nil {
		return postResult{}, err
	}
	if cp.csrf == "" {
		return postResult{}, fmt.Errorf("%w: nexus: no comment form token (signed out, or comments locked)", provider.ErrRelogin)
	}
	// addNewComment() of the site: the text is encodeURIComponent'd inside the form.
	form := url.Values{"game_id": {strconv.Itoa(gid)}, "object_id": {strconv.Itoa(mod)}, "thread_id": {strconv.Itoa(tid)},
		"post": {encodeURIComponent(text)}, "use_emo": {"0"}, "parent_id": {strconv.Itoa(parent)}, "_token": {cp.csrf}}
	r, err := n.submit(ctx, "/mod/comment", n.modPage(game, mod), form)
	switch {
	case err != nil:
		return postResult{}, err
	case r.Status == http.StatusOK && strings.TrimSpace(r.Body) == "1":
		return postResult{Posted: true}, nil // no id: the caller reads it back
	case refused(r.Status):
		return postResult{}, n.refusal(r)
	}
	return postResult{}, fmt.Errorf("%w: HTTP %d: %s", errWriteUnsure, r.Status, snippet([]byte(r.Body)))
}

func (n *native) replyBug(ctx context.Context, issue int, text string) (postResult, error) {
	if err := n.signedIn(); err != nil {
		return postResult{}, err
	}
	bp, err := n.bugReplies(ctx, issue)
	if err != nil {
		return postResult{}, err
	}
	if bp.token == "" {
		return postResult{}, fmt.Errorf("%w: nexus: no reply form token (signed out, issue closed, or not visible)", provider.ErrRelogin)
	}
	r, err := n.submit(ctx, fmt.Sprintf("/mod_bug/%d/reply", issue), "", url.Values{"content": {text}, "_token": {bp.token}})
	if err != nil {
		return postResult{}, err
	}
	var j struct {
		Status  *bool  `json:"status"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(r.Body), &j)
	switch {
	case r.Status == http.StatusOK && j.Status != nil && *j.Status:
		return postResult{Posted: true}, nil
	case r.Status == http.StatusOK && j.Status != nil && !*j.Status:
		return postResult{}, fmt.Errorf("nexus: reply refused: %s", j.Message)
	case refused(r.Status):
		return postResult{}, n.refusal(r)
	}
	// Seen live: saved, yet HTTP 500 (the new reply tile fails to render).
	return postResult{}, fmt.Errorf("%w: HTTP %d: %s", errWriteUnsure, r.Status, snippet([]byte(r.Body)))
}

func (n *native) refusal(r browser.Response) error {
	if r.Status == http.StatusUnauthorized || r.Status == 419 {
		return fmt.Errorf("%w: nexus: HTTP %d", provider.ErrRelogin, r.Status)
	}
	return fmt.Errorf("nexus: refused: HTTP %d: %s", r.Status, snippet([]byte(r.Body)))
}

// encodeURIComponent is JavaScript's encodeURIComponent.
func encodeURIComponent(s string) string {
	const keep = "-_.!~*'()"
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// ── account / sign-in ────────────────────────────────────────────

// whoAmI is the member of a session: api-router preferences{id} (a base64
// gid://…/<memberId>) + user(id){name}.
func (n *native) whoAmI(ctx context.Context, jar *websession.Jar) (uploader, error) {
	var p struct {
		Preferences *struct {
			ID string `json:"id"`
		} `json:"preferences"`
	}
	if err := n.graphql(ctx, n.opts.APIRouter, jar, "Preferences", "query Preferences { preferences { id } }", nil, &p); err != nil {
		return uploader{}, err
	}
	if p.Preferences == nil {
		return uploader{}, fmt.Errorf("%w: nexus: no preferences (signed out)", provider.ErrNotSignedIn)
	}
	id := memberIDFromGID(p.Preferences.ID)
	if id == 0 {
		return uploader{}, errors.New("nexus: account id not found in preferences")
	}
	var u struct {
		User *struct {
			MemberID int    `json:"memberId"`
			Name     string `json:"name"`
		} `json:"user"`
	}
	if err := n.graphql(ctx, n.opts.APIRouter, jar, "UserName", "query UserName($id: Int!) { user(id: $id) { memberId name } }", map[string]any{"id": id}, &u); err != nil {
		return uploader{}, err
	}
	if u.User == nil || u.User.Name == "" {
		return uploader{}, fmt.Errorf("nexus: no user %d", id)
	}
	return uploader{id: id, name: u.User.Name}, nil
}

var gidRe = regexp.MustCompile(`^gid://[^/]+/[^/]+/(\d+)$`)

// memberIDFromGID decodes preferences.id (0 = none).
func memberIDFromGID(id string) int {
	raw := id
	if !strings.HasPrefix(id, "gid://") {
		b, err := base64.StdEncoding.DecodeString(id)
		if err != nil {
			if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(id, "=")); err != nil {
				return 0
			}
		}
		raw = string(b)
	}
	m := gidRe.FindStringSubmatch(raw)
	if m == nil {
		return 0
	}
	v, err := strconv.Atoi(m[1])
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

func (n *native) login(ctx context.Context) (provider.Login, *uploader, error) {
	if n.opts.Session == nil {
		return provider.Login{}, nil, errors.New("nexus: sign-in unavailable")
	}
	l, err := n.opts.Session.Login(ctx)
	if err != nil {
		return provider.Login{}, nil, err
	}
	return l, n.member(l), nil
}

func (n *native) loginStatus(context.Context) (provider.Login, *uploader, error) {
	if n.opts.Session == nil {
		return provider.Login{}, nil, nil
	}
	l := n.opts.Session.Status()
	return l, n.member(l), nil
}

// member is the signed-in account as an uploader (by name: member ids are
// learnt by Account through the listing).
func (n *native) member(l provider.Login) *uploader {
	if !l.LoggedIn || l.Account == "" {
		return nil
	}
	return &uploader{name: l.Account}
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
