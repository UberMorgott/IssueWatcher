package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrNoBrowser means no Chromium-based browser is installed.
var ErrNoBrowser = errors.New("browser: no Chromium-based browser installed (Edge, Chrome, …)")

// ErrChallenge means the site answered a Cloudflare challenge the headless
// browser did not pass.
var ErrChallenge = errors.New("browser: the site asks for a browser check (Cloudflare)")

// ErrOrigin means a request left the allowed origins (a redirect or a caller bug).
var ErrOrigin = errors.New("browser: origin not allowed")

// Exe is an installed browser.
type Exe struct {
	Path string
	Name string // Chrome, Edge, Cent Browser, …
}

// process is a started browser.
type process struct {
	t      Transport
	kill   func()
	exited chan struct{}
}

// Options configures a Browser.
type Options struct {
	// Dir is the browser profile (created owner-only by the caller).
	Dir string
	// Find picks the browser (default Find).
	Find func() (Exe, error)
	// Origins are the only https origins Fetch and pages may use.
	Origins []string
	// IdleStop stops the browser after this long without use (default 3 min).
	IdleStop time.Duration
	// LoadTimeout bounds a page load (default 45 s).
	LoadTimeout time.Duration
	// ChallengeWait is how long a page may show a Cloudflare check before
	// ErrChallenge (default 20 s).
	ChallengeWait time.Duration
	Log           *slog.Logger
	// Args are extra command-line switches (tests: a self-signed page).
	Args []string
	// Start replaces launching a real browser (tests: a fake CDP peer).
	Start func(exe string, args []string) (*process, error)
}

// Browser is one lazily started browser process for every platform.
type Browser struct {
	opts Options
	// stopWait bounds each shutdown step (Browser.close, the exit wait).
	stopWait time.Duration

	op sync.Mutex // one operation at a time (tabs, mode switches)

	mu     sync.Mutex // guards the fields below; never held across CDP calls
	proc   *process
	conn   *Conn
	headed bool
	exe    Exe
	ua     *uaOverride
	tabs   map[string]*tab // origin → loaded tab
	idle   *time.Timer
	gen    int
	closed bool
}

type tab struct {
	target, session string
}

// New creates a Browser; nothing starts until the first call.
func New(opts Options) *Browser {
	if opts.Find == nil {
		opts.Find = Find
	}
	if opts.IdleStop <= 0 {
		opts.IdleStop = 3 * time.Minute
	}
	if opts.LoadTimeout <= 0 {
		opts.LoadTimeout = 45 * time.Second
	}
	if opts.ChallengeWait <= 0 {
		opts.ChallengeWait = 20 * time.Second
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Start == nil {
		opts.Start = startBrowser
	}
	return &Browser{opts: opts, stopWait: 3 * time.Second, tabs: map[string]*tab{}}
}

// Running reports whether a browser process is up.
func (b *Browser) Running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.proc != nil
}

// Headed reports whether the visible (sign-in) browser is up.
func (b *Browser) Headed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.proc != nil && b.headed
}

// Name is the browser in use ("" before the first start).
func (b *Browser) Name() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exe.Name
}

// Close stops the browser for good.
func (b *Browser) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.Stop()
}

// Stop ends the browser process (the next call starts it again).
func (b *Browser) Stop() { b.stop(context.Background()) }

func (b *Browser) stop(ctx context.Context) {
	b.mu.Lock()
	p, c := b.proc, b.conn
	b.proc, b.conn, b.ua = nil, nil, nil
	b.tabs = map[string]*tab{}
	b.gen++
	if b.idle != nil {
		b.idle.Stop()
		b.idle = nil
	}
	b.mu.Unlock()
	b.shutdown(ctx, p, c)
}

// shutdown ends a browser run: Browser.close (bounded) lets it flush the
// profile (cookies); the transport close and the process-tree kill follow
// whether or not it answered, each bounded, so a stuck browser cannot hang it.
func (b *Browser) shutdown(ctx context.Context, p *process, c *Conn) {
	if c != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.stopWait)
		_ = c.Call(cctx, "", "Browser.close", nil, nil)
		cancel()
		closed := make(chan struct{})
		go func() {
			_ = c.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(b.stopWait):
		}
	}
	if p != nil {
		select {
		case <-p.exited:
		case <-time.After(b.stopWait):
		}
		p.kill()
	}
}

// ensure returns the connection in the wanted mode, starting or switching
// the browser (one profile: headless and headed never run together). b.op held.
func (b *Browser) ensure(ctx context.Context, headed bool) (*Conn, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}
	c, cur := b.conn, b.headed
	if c != nil {
		select {
		case <-c.Done(): // crashed or exited: start afresh
			c = nil
		default:
		}
	}
	b.mu.Unlock()
	if c != nil && cur == headed {
		return c, nil
	}
	b.stop(ctx)
	exe, err := b.opts.Find()
	if err != nil {
		return nil, err
	}
	args := []string{
		"--user-data-dir=" + b.opts.Dir, "--no-first-run", "--no-default-browser-check",
		"--disable-blink-features=AutomationControlled", "--disable-sync", "--window-size=1200,860",
	}
	if !headed {
		args = append(args, "--headless=new")
	}
	args = append(args, b.opts.Args...)
	args = append(args, "about:blank")
	p, err := b.opts.Start(exe.Path, args)
	if err != nil {
		return nil, err
	}
	c = NewConn(p.t)
	b.mu.Lock()
	if b.closed { // Close ran during the launch: it saw no process, so kill this one here
		b.mu.Unlock()
		b.shutdown(ctx, p, c)
		return nil, ErrClosed
	}
	b.proc, b.conn, b.headed, b.exe = p, c, headed, exe
	b.tabs = map[string]*tab{}
	b.gen++
	gen := b.gen
	b.mu.Unlock()
	if !headed {
		ua, err := b.userAgent(ctx, c)
		if err != nil {
			b.stop(ctx)
			return nil, err
		}
		b.mu.Lock()
		if b.gen != gen { // stopped or closed meanwhile: that run is gone
			b.mu.Unlock()
			return nil, ErrClosed
		}
		b.ua = ua
		b.mu.Unlock()
	}
	b.opts.Log.Info("browser started", "browser", exe.Name, "headed", headed)
	return c, nil
}

// uaOverride hides "HeadlessChrome" (sites challenge it).
type uaOverride struct {
	UserAgent      string         `json:"userAgent"`
	AcceptLanguage string         `json:"acceptLanguage,omitempty"`
	Platform       string         `json:"platform,omitempty"`
	Metadata       map[string]any `json:"userAgentMetadata,omitempty"`
}

var majorRe = regexp.MustCompile(`/(\d+)\.`)

func (b *Browser) userAgent(ctx context.Context, c *Conn) (*uaOverride, error) {
	var v struct {
		Product   string `json:"product"`
		UserAgent string `json:"userAgent"`
	}
	if err := c.Call(ctx, "", "Browser.getVersion", nil, &v); err != nil {
		return nil, err
	}
	ua := strings.ReplaceAll(v.UserAgent, "HeadlessChrome", "Chrome")
	major := "0"
	if m := majorRe.FindStringSubmatch(v.Product); m != nil {
		major = m[1]
	}
	full := strings.TrimPrefix(strings.TrimPrefix(v.Product, "HeadlessChrome/"), "Chrome/")
	brands := []map[string]string{{"brand": "Chromium", "version": major}, {"brand": "Google Chrome", "version": major}, {"brand": "Not.A/Brand", "version": "99"}}
	return &uaOverride{UserAgent: ua, AcceptLanguage: "en-US,en;q=0.9", Platform: "Win32", Metadata: map[string]any{
		"brands": brands, "fullVersion": full, "platform": "Windows", "platformVersion": "10.0.0",
		"architecture": "x86", "model": "", "mobile": false, "bitness": "64",
	}}, nil
}

// UserAgent is the user agent pages send (plain HTTP reuses it), "" before a start.
func (b *Browser) UserAgent() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ua != nil {
		return b.ua.UserAgent
	}
	return ""
}

// touch (re)arms the idle stop.
func (b *Browser) touch(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.idle != nil {
		b.idle.Stop()
	}
	gen := b.gen
	b.idle = time.AfterFunc(b.opts.IdleStop, func() {
		b.mu.Lock()
		same := b.gen == gen && b.proc != nil && !b.headed
		b.mu.Unlock()
		if same && b.op.TryLock() {
			defer b.op.Unlock()
			b.opts.Log.Debug("browser idle: stopped")
			b.stop(context.WithoutCancel(ctx))
		}
	})
}

// allowed reports whether u is https on an allowed origin.
func (b *Browser) allowed(u string) (string, error) {
	p, err := url.Parse(u)
	if err != nil || !strings.EqualFold(p.Scheme, "https") || p.Host == "" {
		return "", fmt.Errorf("%w: %s", ErrOrigin, u)
	}
	origin := "https://" + strings.ToLower(p.Host)
	for _, o := range b.opts.Origins {
		if strings.EqualFold(o, origin) {
			return origin, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrOrigin, origin)
}

// openTab gives a tab showing page (on origin), loading it once per browser run.
func (b *Browser) openTab(ctx context.Context, c *Conn, origin, page string) (*tab, error) {
	b.mu.Lock()
	t := b.tabs[origin]
	ua := b.ua
	b.mu.Unlock()
	if t != nil {
		return t, nil
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
		return nil, err
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": created.TargetID, "flatten": true}, &att); err != nil {
		return nil, err
	}
	t = &tab{target: created.TargetID, session: att.SessionID}
	if ua != nil {
		if err := c.Call(ctx, t.session, "Emulation.setUserAgentOverride", ua, nil); err != nil {
			return nil, err
		}
	}
	if err := b.load(ctx, c, t, page); err != nil {
		_ = c.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": t.target}, nil)
		return nil, err
	}
	b.mu.Lock()
	b.tabs[origin] = t
	b.mu.Unlock()
	return t, nil
}

// load navigates t to page, waits for the DOM (ads may hold the load event for long), then for a Cloudflare
// check to clear (ErrChallenge when it does not).
func (b *Browser) load(ctx context.Context, c *Conn, t *tab, page string) error {
	if err := c.Call(ctx, t.session, "Page.enable", nil, nil); err != nil {
		return err
	}
	loaded := make(chan struct{}, 1)
	off := c.On(func(ev Event) {
		if ev.Session == t.session && (ev.Method == "Page.domContentEventFired" || ev.Method == "Page.loadEventFired") {
			select {
			case loaded <- struct{}{}:
			default:
			}
		}
	})
	defer off()
	lctx, cancel := context.WithTimeout(ctx, b.opts.LoadTimeout)
	defer cancel()
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if err := c.Call(lctx, t.session, "Page.navigate", map[string]any{"url": page}, &nav); err != nil {
		return err
	}
	if nav.ErrorText != "" {
		return fmt.Errorf("browser: load %s: %s", page, nav.ErrorText)
	}
	select {
	case <-loaded:
	case <-lctx.Done():
		return fmt.Errorf("browser: load %s: %w", page, lctx.Err())
	case <-c.Done():
		return c.Err()
	}
	deadline := time.Now().Add(b.opts.ChallengeWait)
	for {
		var title string
		if err := b.eval(ctx, c, t, "document.title", &title); err != nil {
			return err
		}
		if !challengeTitle(title) {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrChallenge
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func challengeTitle(t string) bool {
	t = strings.ToLower(t)
	return strings.Contains(t, "just a moment") || strings.Contains(t, "attention required") || strings.Contains(t, "один момент")
}

// eval runs expression in t (promises awaited) and decodes its JSON value.
func (b *Browser) eval(ctx context.Context, c *Conn, t *tab, expression string, out any) error {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := c.Call(ctx, t.session, "Runtime.evaluate", map[string]any{
		"expression": expression, "awaitPromise": true, "returnByValue": true,
	}, &r)
	if err != nil {
		return err
	}
	if r.Exception != nil {
		msg := r.Exception.Text
		if r.Exception.Exception != nil && r.Exception.Exception.Description != "" {
			msg = r.Exception.Exception.Description
		}
		return fmt.Errorf("browser: page script: %s", firstLine(msg))
	}
	if out != nil && len(r.Result.Value) > 0 {
		return json.Unmarshal(r.Result.Value, out)
	}
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// Request is an in-page fetch (credentials included, as the site's own scripts).
type Request struct {
	URL     string
	Method  string // default GET
	Headers map[string]string
	Body    string
	// Page is loaded first when the origin has no tab yet (default origin + "/"):
	// a page that passes the site's browser check.
	Page string
}

// Response of a Fetch.
type Response struct {
	Status      int    `json:"status"`
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
	Body        string `json:"body"`
}

const fetchJS = `(async (a) => {
  const r = await fetch(a.url, {method: a.method, headers: a.headers, body: a.body === "" ? undefined : a.body, credentials: "include", redirect: "follow"});
  return {status: r.status, url: r.url, contentType: r.headers.get("content-type") || "", body: await r.text()};
})`

// Fetch runs req inside a page of its origin (the browser's cookies, TLS and
// Cloudflare clearance). Only allowed https origins; a redirect elsewhere
// fails with ErrOrigin.
func (b *Browser) Fetch(ctx context.Context, req Request) (Response, error) {
	origin, err := b.allowed(req.URL)
	if err != nil {
		return Response{}, err
	}
	page := req.Page
	if page == "" {
		page = origin + "/"
	}
	if po, err := b.allowed(page); err != nil || po != origin {
		return Response{}, fmt.Errorf("%w: page %s", ErrOrigin, page)
	}
	if req.Method == "" {
		req.Method = "GET"
	}
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}
	args, err := json.Marshal(map[string]any{"url": req.URL, "method": req.Method, "headers": req.Headers, "body": req.Body})
	if err != nil {
		return Response{}, err
	}
	b.op.Lock()
	defer b.op.Unlock()
	defer b.touch(ctx)
	c, err := b.ensure(ctx, b.Headed())
	if err != nil {
		return Response{}, err
	}
	t, err := b.openTab(ctx, c, origin, page)
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := b.eval(ctx, c, t, fetchJS+"("+string(args)+")", &resp); err != nil {
		b.dropTab(origin) // a crashed / navigated page is reloaded next time
		return Response{}, err
	}
	if resp.URL != "" {
		if o, err := b.allowed(resp.URL); err != nil || o != origin {
			return Response{}, fmt.Errorf("%w: redirected to %s", ErrOrigin, resp.URL)
		}
	}
	if resp.Status == 403 && challengeBody(resp.Body) {
		b.dropTab(origin) // the clearance expired: the page is loaded (and checked) again
		return resp, ErrChallenge
	}
	return resp, nil
}

func challengeBody(s string) bool {
	if len(s) > 4096 {
		s = s[:4096]
	}
	return challengeTitle(s) || strings.Contains(s, "cf-chl") || strings.Contains(s, "challenge-platform")
}

func (b *Browser) dropTab(origin string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.tabs, origin)
}

// Cookie is a browser cookie (Storage.getCookies).
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"` // Unix seconds; -1 = session
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	Session  bool    `json:"session"`
}

// Cookies returns the profile's cookies of domains (and their subdomains),
// from the running browser (started headless when none is).
func (b *Browser) Cookies(ctx context.Context, domains []string) ([]Cookie, error) {
	b.op.Lock()
	defer b.op.Unlock()
	defer b.touch(ctx)
	c, err := b.ensure(ctx, b.Headed())
	if err != nil {
		return nil, err
	}
	return cookiesOf(ctx, c, domains)
}

func cookiesOf(ctx context.Context, c *Conn, domains []string) ([]Cookie, error) {
	var r struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := c.Call(ctx, "", "Storage.getCookies", nil, &r); err != nil {
		return nil, err
	}
	var out []Cookie
	for _, ck := range r.Cookies {
		if DomainMatch(ck.Domain, domains) {
			out = append(out, ck)
		}
	}
	return out, nil
}

// DomainMatch reports whether a cookie domain (".example.com" or
// "www.example.com") belongs to one of domains or their subdomains.
func DomainMatch(cookieDomain string, domains []string) bool {
	d := strings.ToLower(strings.TrimPrefix(cookieDomain, "."))
	for _, want := range domains {
		w := strings.ToLower(strings.TrimPrefix(want, "."))
		if d == w || strings.HasSuffix(d, "."+w) {
			return true
		}
	}
	return false
}

// ClearSite deletes the profile's cookies of domains and the storage of
// origins («Выйти»: a later silent capture must not sign in again).
func (b *Browser) ClearSite(ctx context.Context, domains, origins []string) error {
	b.op.Lock()
	defer b.op.Unlock()
	defer b.touch(ctx)
	c, err := b.ensure(ctx, b.Headed())
	if err != nil {
		return err
	}
	cks, err := cookiesOf(ctx, c, domains)
	if err != nil {
		return err
	}
	for _, ck := range cks {
		if err := c.Call(ctx, "", "Storage.deleteCookies", map[string]any{"name": ck.Name, "domain": ck.Domain, "path": ck.Path}, nil); err != nil {
			// Older builds lack Storage.deleteCookies: expire it instead.
			exp := map[string]any{"name": ck.Name, "value": "", "domain": ck.Domain, "path": ck.Path, "expires": 1, "secure": ck.Secure}
			if err2 := c.Call(ctx, "", "Storage.setCookies", map[string]any{"cookies": []any{exp}}, nil); err2 != nil {
				return err
			}
		}
	}
	for _, o := range origins {
		// Best effort (the session lives in the cookies deleted above; some
		// builds refuse "all").
		types := "local_storage,indexeddb,cache_storage,service_workers,websql,file_systems"
		if err := c.Call(ctx, "", "Storage.clearDataForOrigin", map[string]any{"origin": o, "storageTypes": types}, nil); err != nil {
			b.opts.Log.Debug("browser: clear site storage", "origin", o, "err", err)
		}
	}
	for _, o := range origins {
		b.dropTab(o)
	}
	return nil
}

// OpenWindow switches to the visible browser and opens u in a window
// (the sign-in page); the window stays until Stop or the idle stop after it.
func (b *Browser) OpenWindow(ctx context.Context, u string) error {
	if _, err := b.allowed(u); err != nil {
		return err
	}
	b.op.Lock()
	defer b.op.Unlock()
	c, err := b.ensure(ctx, true)
	if err != nil {
		return err
	}
	var targets struct {
		Infos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := c.Call(ctx, "", "Target.getTargets", nil, &targets); err == nil {
		for _, t := range targets.Infos {
			if t.Type == "page" && t.URL == "about:blank" { // the start page: reuse its window
				var att struct {
					SessionID string `json:"sessionId"`
				}
				if err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": t.TargetID, "flatten": true}, &att); err == nil {
					if err := c.Call(ctx, att.SessionID, "Page.navigate", map[string]any{"url": u}, nil); err == nil {
						_ = c.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": t.TargetID}, nil)
						return nil
					}
				}
			}
		}
	}
	return c.Call(ctx, "", "Target.createTarget", map[string]any{"url": u, "newWindow": true}, nil)
}

// WindowOpen reports whether the visible browser still shows a page (the
// user may close the window: the browser then exits).
func (b *Browser) WindowOpen(ctx context.Context) bool {
	b.mu.Lock()
	c, headed := b.conn, b.headed
	b.mu.Unlock()
	if c == nil || !headed {
		return false
	}
	select {
	case <-c.Done():
		return false
	default:
	}
	var targets struct {
		Infos []struct {
			Type string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := c.Call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
		return false
	}
	for _, t := range targets.Infos {
		if t.Type == "page" {
			return true
		}
	}
	return false
}

// ProfileExists reports whether the profile dir has been used.
func (b *Browser) ProfileExists() bool {
	st, err := os.Stat(b.opts.Dir)
	return err == nil && st.IsDir()
}
