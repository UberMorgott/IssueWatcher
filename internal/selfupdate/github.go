// Package selfupdate replaces the running IssueWatcher executable with a newer
// GitHub release of UberMorgott/IssueWatcher (Windows amd64).
//
// Discovery uses github.com itself (the releases Atom feed and the release
// download URLs), never the REST API: the anonymous API quota (60 an hour per
// IP) is shared with everything else on the network and ran out, which left
// installs stuck on an old version.
//
// Trust: a release is installed only when its manifest.json carries a valid
// Ed25519 signature by the release key compiled into this build (pubkey.go),
// names a version strictly newer than the running one (no downgrades) and
// equal to the release tag, and the downloaded executable matches the
// manifest's size and SHA-256. Nothing on disk is touched before the download
// verified. The feed is only a hint of which tag to try.
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
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Repo is the GitHub repository releases come from.
const Repo = "UberMorgott/IssueWatcher"

// DefaultBase is the GitHub web root. IW_UPDATE_BASE overrides it for tests (a
// local fake of the feed and downloads); a signed manifest is still required,
// so an override cannot install anything the release key did not sign.
const DefaultBase = "https://github.com"

// Channels.
const (
	ChannelStable  = "stable"  // highest release version (no prerelease tag)
	ChannelPreview = "preview" // highest version including prereleases
)

const maxFeedSize = 4 << 20

// Asset is one file of a release. JSON names match the cached update-check.json.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Release is a published GitHub release.
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Notes       string    `json:"body"`
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

// Source reads releases from github.com (or a test fake).
type Source struct {
	Base      string // "" = DefaultBase
	UserAgent string
	Client    *http.Client // nil = a 5-minute-timeout client
}

func (s Source) base() string { return strings.TrimRight(cmp.Or(s.Base, DefaultBase), "/") }

// official reports whether s talks to the real GitHub (HTTPS enforced).
func (s Source) official() bool { return s.base() == DefaultBase }

func (s Source) userAgent() string { return cmp.Or(s.UserAgent, "issuewatcher-selfupdate") }

func (s Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

type atomFeed struct {
	Entries []struct {
		Title   string    `xml:"title"`
		Updated time.Time `xml:"updated"`
		Content string    `xml:"content"`
		Link    struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

// Latest returns the newest release of channel from the releases Atom feed:
// stable = the highest version without a prerelease part, preview = the
// highest version. (nil, nil) when the repository has no release yet. Drafts
// never appear in the feed.
// shortcut: the feed lists the 10 newest releases, so stable is missed after
// 10 prereleases in a row; upgrade to the releases/latest redirect if that happens.
func (s Source) Latest(ctx context.Context, channel string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base()+"/"+Repo+"/releases.atom", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/atom+xml")
	req.Header.Set("User-Agent", s.userAgent())
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("releases feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases feed: HTTP %d", resp.StatusCode)
	}
	var feed atomFeed
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxFeedSize)).Decode(&feed); err != nil {
		return nil, fmt.Errorf("releases feed: %w", err)
	}
	var best *Release
	for _, e := range feed.Entries {
		_, rawTag, ok := strings.Cut(e.Link.Href, "/releases/tag/")
		tag, err := url.PathUnescape(rawTag)
		if !ok || err != nil || !IsRelease(tag) || (channel != ChannelPreview && IsPrerelease(tag)) {
			continue
		}
		if best != nil && Compare(tag, best.Tag) <= 0 {
			continue
		}
		dl := s.base() + "/" + Repo + "/releases/download/" + url.PathEscape(tag) + "/"
		best = &Release{
			Tag: tag, Name: e.Title, Notes: notesText(e.Content), Prerelease: IsPrerelease(tag),
			PublishedAt: e.Updated, HTMLURL: e.Link.Href,
		}
		for _, n := range []string{ManifestAsset, SignatureAsset, ThisAsset()} {
			best.Assets = append(best.Assets, Asset{Name: n, URL: dl + url.PathEscape(n)})
		}
	}
	return best, nil
}

var (
	blockEndRe = regexp.MustCompile(`(?i)</(p|li|h[1-6]|pre|ul|ol)>|<br\s*/?>`)
	tagRe      = regexp.MustCompile(`<[^>]*>`)
	blankRe    = regexp.MustCompile(`\s*\n\s*`)
)

// notesText turns the feed's rendered release notes (HTML) into plain text for
// the <pre> in Settings.
func notesText(h string) string {
	h = strings.ReplaceAll(h, "<li>", "- ")
	h = blockEndRe.ReplaceAllString(h, "$0\n")
	h = html.UnescapeString(tagRe.ReplaceAllString(h, ""))
	return strings.TrimSpace(blankRe.ReplaceAllString(h, "\n"))
}
