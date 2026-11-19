// Package updatetest fakes GitHub's releases feed and downloads for self-update tests
// (unit tests and the end-to-end run behind IW_UPDATE_BASE).
package updatetest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"

	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
)

// Release is one fake release.
type Release struct {
	Tag   string
	Notes string
	Files map[string][]byte // asset name → content
	// Prerelease is GitHub's prerelease flag (also implied by a prerelease tag):
	// such a release is never releases/latest.
	Prerelease bool
}

// Signed returns the asset files of a release of exe signed with priv; tamper,
// when not nil, edits the executable bytes after signing (a corrupted asset).
func Signed(priv ed25519.PrivateKey, tag string, exe []byte, tamper func([]byte) []byte) map[string][]byte {
	sum := sha256.Sum256(exe)
	m := selfupdate.Manifest{Version: tag, Asset: selfupdate.ThisAsset(), Size: int64(len(exe)), SHA256: hex.EncodeToString(sum[:])}
	body, err := m.Encode()
	if err != nil {
		panic(err)
	}
	if tamper != nil {
		exe = tamper(slices.Clone(exe))
	}
	return map[string][]byte{
		m.Asset:                   exe,
		selfupdate.ManifestAsset:  body,
		selfupdate.SignatureAsset: selfupdate.Sign(priv, body),
	}
}

// Fake serves github.com's /<repo>/releases.atom, /<repo>/releases/latest and
// /<repo>/releases/download/<tag>/<name>.
type Fake struct {
	mu       sync.Mutex
	releases []Release
	handler  http.Handler
}

// NewFake returns an empty fake; mount Handler on a server.
func NewFake() *Fake {
	f := &Fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+selfupdate.Repo+"/releases.atom", f.feed)
	mux.HandleFunc("GET /"+selfupdate.Repo+"/releases/latest", f.latest)
	mux.HandleFunc("GET /"+selfupdate.Repo+"/releases/download/{tag}/{name}", f.download)
	f.handler = mux
	return f
}

// Handler is the fake site.
func (f *Fake) Handler() http.Handler { return f.handler }

// Start serves the fake on a loopback httptest server.
func (f *Fake) Start() *httptest.Server { return httptest.NewServer(f.handler) }

// Set replaces all releases.
func (f *Fake) Set(rs ...Release) {
	f.mu.Lock()
	f.releases = rs
	f.mu.Unlock()
}

// feed writes the releases as GitHub's Atom feed (notes HTML-escaped in
// <content type="html">, like the real one).
func (f *Fake) feed(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom">`)
	for _, rel := range f.releases {
		fmt.Fprintf(&b, `<entry><updated>2026-09-28T12:00:00Z</updated><link rel="alternate" type="text/html" href="http://%s/%s/releases/tag/%s"/><title>IssueWatcher %s</title><content type="html">%s</content></entry>`,
			r.Host, selfupdate.Repo, rel.Tag, rel.Tag, esc(html.EscapeString(rel.Notes)))
	}
	b.WriteString(`</feed>`)
	_, _ = w.Write([]byte(b.String()))
}

// latest redirects like github.com: to the highest non-prerelease release's
// tag page, or to the releases list when there is none.
func (f *Fake) latest(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	best := ""
	for _, rel := range f.releases {
		if !rel.Prerelease && !selfupdate.IsPrerelease(rel.Tag) && (best == "" || selfupdate.Compare(rel.Tag, best) > 0) {
			best = rel.Tag
		}
	}
	to := "/" + selfupdate.Repo + "/releases"
	if best != "" {
		to += "/tag/" + best
	}
	http.Redirect(w, r, "http://"+r.Host+to, http.StatusFound)
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func (f *Fake) download(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tag, name := r.PathValue("tag"), r.PathValue("name")
	for _, rel := range f.releases {
		if rel.Tag == tag {
			if b, ok := rel.Files[name]; ok && !strings.Contains(name, "/") {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(b)
				return
			}
		}
	}
	http.NotFound(w, r)
}
