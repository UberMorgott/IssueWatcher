// Package updatetest fakes the GitHub releases API for self-update tests
// (unit tests and the end-to-end run behind IW_UPDATE_BASE).
package updatetest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
)

// Release is one fake release.
type Release struct {
	Tag        string
	Prerelease bool
	Notes      string
	Files      map[string][]byte // asset name → content
	NoDigest   bool              // omit GitHub's asset digest (tests the manifest check alone)
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

// Fake serves /repos/<repo>/releases[/latest] and /download/<tag>/<name>.
type Fake struct {
	mu       sync.Mutex
	releases []Release
	handler  http.Handler
}

// NewFake returns an empty fake; mount Handler on a server.
func NewFake() *Fake {
	f := &Fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+selfupdate.Repo+"/releases/latest", f.latest)
	mux.HandleFunc("GET /repos/"+selfupdate.Repo+"/releases", f.list)
	mux.HandleFunc("GET /download/{tag}/{name}", f.download)
	f.handler = mux
	return f
}

// Handler is the fake API.
func (f *Fake) Handler() http.Handler { return f.handler }

// Start serves the fake on a loopback httptest server.
func (f *Fake) Start() *httptest.Server { return httptest.NewServer(f.handler) }

// Set replaces all releases.
func (f *Fake) Set(rs ...Release) {
	f.mu.Lock()
	f.releases = rs
	f.mu.Unlock()
}

func base(r *http.Request) string { return "http://" + r.Host }

func (f *Fake) json(r *http.Request, rel Release) selfupdate.Release {
	out := selfupdate.Release{
		Tag: rel.Tag, Name: "IssueWatcher " + rel.Tag, Notes: rel.Notes, Prerelease: rel.Prerelease,
		PublishedAt: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC), HTMLURL: base(r) + "/release/" + rel.Tag,
	}
	names := make([]string, 0, len(rel.Files))
	for n := range rel.Files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		b := rel.Files[n]
		a := selfupdate.Asset{Name: n, Size: int64(len(b)), URL: base(r) + "/download/" + rel.Tag + "/" + n}
		if !rel.NoDigest {
			sum := sha256.Sum256(b)
			a.Digest = "sha256:" + hex.EncodeToString(sum[:])
		}
		out.Assets = append(out.Assets, a)
	}
	return out
}

func (f *Fake) latest(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *Release
	for i := range f.releases {
		rel := &f.releases[i]
		if !rel.Prerelease && (best == nil || selfupdate.Compare(rel.Tag, best.Tag) > 0) {
			best = rel
		}
	}
	if best == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, f.json(r, *best))
}

func (f *Fake) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []selfupdate.Release{}
	for _, rel := range f.releases {
		out = append(out, f.json(r, rel))
	}
	writeJSON(w, out)
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
