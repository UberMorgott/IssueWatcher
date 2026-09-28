// Package selfupdate replaces the running IssueWatcher executable with a newer
// GitHub release of UberMorgott/IssueWatcher (Windows amd64).
//
// Trust: a release is installed only when its manifest.json carries a valid
// Ed25519 signature by the release key compiled into this build (pubkey.go),
// names a version strictly newer than the running one (no downgrades) and
// equal to the release tag, and the downloaded executable matches the
// manifest's size and SHA-256 (and GitHub's own asset digest when the API
// reports one). Nothing on disk is touched before the download verified.
//
// Swap (the Windows pattern proven in agent-link): the running exe is renamed
// to ".<name>.old" (a running exe cannot be overwritten, only renamed), the
// verified ".<name>.new" takes its place, and the new exe is started with
// --after-update=<pid>. The new process signals ready (data\update-ready),
// waits for the old PID to exit, takes over the same loopback port and deletes
// the leftovers. The old process shuts down gracefully once the new one is
// ready; if it never gets ready, the swap is rolled back.
package selfupdate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Repo is the GitHub repository releases come from.
const Repo = "UberMorgott/IssueWatcher"

// DefaultAPIBase is the GitHub REST root. IW_UPDATE_BASE overrides it for
// tests (a local fake of the releases API); a signed manifest is still
// required, so an override cannot install anything the release key did not sign.
const DefaultAPIBase = "https://api.github.com"

// Channels.
const (
	ChannelStable  = "stable"  // latest non-prerelease release
	ChannelPreview = "preview" // newest release including prereleases
)

const maxJSONSize = 4 << 20

// Asset is one file of a release.
type Asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"` // "sha256:<hex>", computed by GitHub on upload
}

// Release is a published GitHub release.
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Notes       string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

func (r *Release) asset(name string) (Asset, bool) {
	i := slices.IndexFunc(r.Assets, func(a Asset) bool { return a.Name == name })
	if i < 0 {
		return Asset{}, false
	}
	return r.Assets[i], true
}

// Source reads releases from the GitHub REST API (or a test fake).
type Source struct {
	APIBase   string // "" = DefaultAPIBase
	UserAgent string
	Client    *http.Client // nil = a 5-minute-timeout client
}

func (s Source) base() string { return strings.TrimRight(cmp.Or(s.APIBase, DefaultAPIBase), "/") }

// official reports whether s talks to the real GitHub (HTTPS enforced).
func (s Source) official() bool { return s.base() == DefaultAPIBase }

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// Latest returns the newest release of channel: stable = GitHub's "latest"
// release (never a prerelease or draft); preview = the highest version among
// the recent non-draft releases, prereleases included. (nil, nil) when the
// repository has no release yet.
func (s Source) Latest(ctx context.Context, channel string) (*Release, error) {
	if channel != ChannelPreview {
		var r Release
		found, err := s.getJSON(ctx, "/repos/"+Repo+"/releases/latest", &r)
		if err != nil || !found {
			return nil, err
		}
		return &r, nil
	}
	var list []Release
	found, err := s.getJSON(ctx, "/repos/"+Repo+"/releases?per_page=30", &list)
	if err != nil || !found {
		return nil, err
	}
	var best *Release
	for i := range list {
		r := &list[i]
		if r.Draft || !IsRelease(r.Tag) {
			continue
		}
		if best == nil || Compare(r.Tag, best.Tag) > 0 {
			best = r
		}
	}
	return best, nil
}

// getJSON GETs path into v; found is false for a 404.
func (s Source) getJSON(ctx context.Context, path string, v any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base()+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", cmp.Or(s.UserAgent, "issuewatcher-selfupdate"))
	resp, err := s.client().Do(req)
	if err != nil {
		return false, fmt.Errorf("releases API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONSize))
	if err != nil {
		return false, fmt.Errorf("releases API: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0":
		return false, ErrRateLimited
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("releases API: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return false, fmt.Errorf("releases API: %w", err)
	}
	return true, nil
}

// ErrRateLimited is GitHub's unauthenticated limit (60 requests an hour per IP).
var ErrRateLimited = errors.New("GitHub rate limit reached for this network; try again later")
