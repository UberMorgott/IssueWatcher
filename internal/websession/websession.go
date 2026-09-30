// Package websession is a platform's stored web session: its cookie jar
// (encrypted per platform, data\secrets\<platform>.json via DPAPI) and a plain
// HTTP client that sends those cookies with the browser's user agent, keeps
// Set-Cookie answers, stays on the platform's hosts and reports Cloudflare
// challenges (docs/ARCHITECTURE.md → Native mod platforms). Cookie values are
// never logged.
package websession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/secret"
)

// ErrChallenge means the site answered a Cloudflare challenge (plain HTTP
// cannot pass it; the caller switches to the browser).
var ErrChallenge = errors.New("websession: Cloudflare challenge")

// ErrHost means a request or redirect left the platform's hosts.
var ErrHost = errors.New("websession: host not allowed")

// MaxBody bounds one answer.
const MaxBody = 32 << 20

// Cookie is one stored cookie. Domain with a leading dot matches subdomains;
// without, the exact host only.
type Cookie struct {
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Domain  string    `json:"domain"`
	Path    string    `json:"path"`
	Expires time.Time `json:"expires,omitzero"` // zero = session cookie
	Secure  bool      `json:"secure,omitempty"`
}

type jarFile struct {
	Cookies   []Cookie  `json:"cookies"`
	UserAgent string    `json:"userAgent,omitempty"`
	Account   string    `json:"account,omitempty"`
	Source    string    `json:"source,omitempty"` // window | profile | stored
	SavedAt   time.Time `json:"savedAt,omitzero"`
}

// Jar is a platform's persisted cookies.
type Jar struct {
	path string
	now  func() time.Time

	mu sync.Mutex
	f  jarFile
}

// Open loads the jar at path (missing = empty).
func Open(path string) (*Jar, error) {
	j := &Jar{path: path, now: time.Now}
	err := secret.ReadProtectedJSON(path, &j.f)
	if err != nil && !errors.Is(err, secret.ErrNotFound) {
		return j, err
	}
	return j, nil
}

// Memory is an unsaved jar (a candidate session under test).
func Memory(cookies []Cookie, userAgent string) *Jar {
	return &Jar{now: time.Now, f: jarFile{Cookies: cookies, UserAgent: userAgent}}
}

// Cookies are the stored cookies (a copy).
func (j *Jar) Cookies() []Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Cookie(nil), j.f.Cookies...)
}

func (j *Jar) save() error {
	if j.path == "" {
		return nil
	}
	return secret.WriteProtectedJSON(j.path, j.f)
}

// Empty reports whether no live cookie is stored.
func (j *Jar) Empty() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	for _, c := range j.f.Cookies {
		if c.Expires.IsZero() || c.Expires.After(now) {
			return false
		}
	}
	return true
}

// UserAgent is the browser user agent the cookies were captured with.
func (j *Jar) UserAgent() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.UserAgent
}

// Account is the identity the session was captured as ("" = unknown).
func (j *Jar) Account() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Account
}

// Source is where the session came from: "window" (the sign-in window) or
// "profile" (our browser profile was already signed in).
func (j *Jar) Source() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Source
}

// Replace stores a new session (the sign-in capture).
func (j *Jar) Replace(cookies []Cookie, userAgent, account, source string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.f = jarFile{Cookies: cookies, UserAgent: userAgent, Account: account, Source: source, SavedAt: j.now().UTC()}
	return j.save()
}

// SetAccount records the identity of the stored session.
func (j *Jar) SetAccount(account string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f.Account == account {
		return nil
	}
	j.f.Account = account
	return j.save()
}

// Clear drops the session and its file.
func (j *Jar) Clear() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.f = jarFile{}
	if j.path == "" {
		return nil
	}
	return secret.Remove(j.path)
}

// Value is a stored cookie's value ("" = none), e.g. an XSRF token.
func (j *Jar) Value(name string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range j.f.Cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// Header is the Cookie header for u (live cookies of its host and path;
// secure ones over https only).
func (j *Jar) Header(u *url.URL) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	host := strings.ToLower(u.Hostname())
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	var parts []string
	for _, c := range j.f.Cookies {
		if (!c.Expires.IsZero() && !c.Expires.After(now)) || (c.Secure && u.Scheme != "https") ||
			!hostMatch(host, c.Domain) || !pathMatch(path, c.Path) {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func hostMatch(host, domain string) bool {
	d := strings.ToLower(domain)
	if base, ok := strings.CutPrefix(d, "."); ok {
		return host == base || strings.HasSuffix(host, "."+base)
	}
	return host == d
}

func pathMatch(path, cp string) bool {
	if cp == "" || cp == "/" {
		return true
	}
	return path == cp || strings.HasPrefix(path, strings.TrimSuffix(cp, "/")+"/")
}

// Ingest keeps the Set-Cookie answers of a response to u (saved when changed).
func (j *Jar) Ingest(u *url.URL, resp *http.Response) error {
	set := resp.Cookies()
	if len(set) == 0 {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.f.Cookies) == 0 {
		return nil // no session: anonymous answers are not kept
	}
	now := j.now()
	host := strings.ToLower(u.Hostname())
	changed := false
	for _, sc := range set {
		c := Cookie{Name: sc.Name, Value: sc.Value, Path: sc.Path, Secure: sc.Secure, Domain: host}
		if sc.Domain != "" {
			d := strings.ToLower(strings.TrimPrefix(sc.Domain, "."))
			if host != d && !strings.HasSuffix(host, "."+d) {
				continue // a cookie for a foreign domain: refused, as a browser does
			}
			c.Domain = "." + d
		}
		if c.Path == "" {
			c.Path = "/"
		}
		switch {
		case sc.MaxAge < 0:
			c.Expires = now.Add(-time.Second)
		case sc.MaxAge > 0:
			c.Expires = now.Add(time.Duration(sc.MaxAge) * time.Second)
		case !sc.Expires.IsZero():
			c.Expires = sc.Expires.UTC()
		}
		i := j.index(c)
		expired := !c.Expires.IsZero() && !c.Expires.After(now)
		switch {
		case expired && i >= 0:
			j.f.Cookies = append(j.f.Cookies[:i], j.f.Cookies[i+1:]...)
			changed = true
		case expired:
		case i >= 0:
			if j.f.Cookies[i] != c {
				j.f.Cookies[i] = c
				changed = true
			}
		default:
			j.f.Cookies = append(j.f.Cookies, c)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return j.save()
}

func (j *Jar) index(c Cookie) int {
	for i, x := range j.f.Cookies {
		if x.Name == c.Name && strings.EqualFold(x.Domain, c.Domain) && x.Path == c.Path {
			return i
		}
	}
	return -1
}

// FromBrowser converts captured browser cookies.
func FromBrowser(cks []browser.Cookie) []Cookie {
	out := make([]Cookie, 0, len(cks))
	for _, c := range cks {
		x := Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure}
		if !c.Session && c.Expires > 0 {
			x.Expires = time.Unix(int64(c.Expires), 0).UTC()
		}
		out = append(out, x)
	}
	return out
}

// Client is plain HTTP on a platform's hosts with its jar.
type Client struct {
	// Jar is the session (nil = anonymous).
	Jar *Jar
	// Hosts are the only hosts requests and redirects may reach (https).
	Hosts []string
	// UserAgent is used when the jar has none.
	UserAgent string
	HTTP      *http.Client
}

// DefaultUserAgent is a desktop Chrome UA for keyless reads before any capture.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// Result is a whole answer.
type Result struct {
	Status int
	Header http.Header
	Body   []byte
	URL    string // final URL after redirects
}

func (c *Client) allowed(u *url.URL) bool {
	if u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, want := range c.Hosts {
		if h == strings.ToLower(want) {
			return true
		}
	}
	return false
}

// Do sends one request (anonymous when anon) and reads the whole answer.
// A challenge answer is ErrChallenge (with the result).
func (c *Client) Do(ctx context.Context, method, rawURL string, header http.Header, body []byte, anon bool) (Result, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !c.allowed(u) {
		return Result{}, fmt.Errorf("%w: %s", ErrHost, rawURL)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return Result{}, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	jar := c.Jar
	if anon {
		jar = nil
	}
	ua := c.UserAgent
	if jar != nil && jar.UserAgent() != "" {
		ua = jar.UserAgent()
	}
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	if jar != nil {
		if h := jar.Header(u); h != "" {
			req.Header.Set("Cookie", h)
		}
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	cl := *hc
	cl.Jar = nil
	cl.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !c.allowed(r.URL) {
			return fmt.Errorf("%w: redirect to %s", ErrHost, r.URL.Redacted())
		}
		// net/http copies the first request's headers to every hop; the Cookie
		// header is never trusted from that copy: it is rebuilt from the jar
		// (after this hop's Set-Cookie) for the destination URL.
		r.Header.Del("Cookie")
		if jar == nil {
			return nil
		}
		if r.Response != nil && len(via) > 0 {
			_ = jar.Ingest(via[len(via)-1].URL, r.Response)
		}
		if h := jar.Header(r.URL); h != "" {
			// Not the blind copy G119 guards against: the header is recomputed from
			// the jar's domain/path/secure rules for this allowed https hop only.
			r.Header.Set("Cookie", h) //nolint:gosec // G119: rebuilt per hop from the jar, host allow-listed above
		}
		return nil
	}
	resp, err := cl.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if jar != nil {
		_ = jar.Ingest(resp.Request.URL, resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	res := Result{Status: resp.StatusCode, Header: resp.Header, Body: b, URL: resp.Request.URL.String()}
	if err != nil {
		return res, err
	}
	if Challenged(resp.StatusCode, resp.Header, b) {
		return res, ErrChallenge
	}
	return res, nil
}

// Challenged reports a Cloudflare challenge answer.
func Challenged(status int, h http.Header, body []byte) bool {
	if strings.EqualFold(h.Get("cf-mitigated"), "challenge") {
		return true
	}
	if status != http.StatusForbidden && status != http.StatusServiceUnavailable && status != 429 {
		return false
	}
	head := body
	if len(head) > 8192 {
		head = head[:8192]
	}
	s := strings.ToLower(string(head))
	return strings.Contains(s, "<title>just a moment") || strings.Contains(s, "challenge-platform") || strings.Contains(s, "cf-chl-")
}
