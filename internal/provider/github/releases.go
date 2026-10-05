package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Releases (autopilot release run): read a release by tag, create one, upload
// its assets. User actions: no quota reserve.

var _ provider.Releaser = (*Provider)(nil)

type restAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	State              string `json:"state"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func (a restAsset) asset() provider.ReleaseAsset {
	return provider.ReleaseAsset{ID: a.ID, Name: a.Name, Size: a.Size, State: a.State, Digest: a.Digest,
		BrowserDownloadURL: a.BrowserDownloadURL}
}

type restRelease struct {
	ID         int64       `json:"id"`
	TagName    string      `json:"tag_name"`
	Name       string      `json:"name"`
	HTMLURL    string      `json:"html_url"`
	UploadURL  string      `json:"upload_url"`
	Draft      bool        `json:"draft"`
	Prerelease bool        `json:"prerelease"`
	Assets     []restAsset `json:"assets"`
}

func (r restRelease) release() provider.Release {
	out := provider.Release{ID: r.ID, TagName: r.TagName, Name: r.Name, HTMLURL: r.HTMLURL, UploadURL: r.UploadURL,
		Draft: r.Draft, Prerelease: r.Prerelease, Assets: make([]provider.ReleaseAsset, 0, len(r.Assets))}
	for _, a := range r.Assets {
		out.Assets = append(out.Assets, a.asset())
	}
	return out
}

func (p *Provider) releasesURL(repo string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/repos/%s/%s/releases", p.auth.APIURL, url.PathEscape(owner), url.PathEscape(name)), nil
}

// GetReleaseByTag returns repo's release for tag; a 404 is found == false.
// Draft releases are not returned by this endpoint.
func (p *Provider) GetReleaseByTag(ctx context.Context, repo, tag string) (provider.Release, bool, error) {
	base, err := p.releasesURL(repo)
	if err != nil {
		return provider.Release{}, false, err
	}
	var out restRelease
	if err := p.c.do(ctx, http.MethodGet, base+"/tags/"+url.PathEscape(tag), nil, &out, 0); err != nil {
		if isNotFound(err) {
			return provider.Release{}, false, nil
		}
		return provider.Release{}, false, err
	}
	return out.release(), true, nil
}

// CreateRelease creates a release for r.TagName.
func (p *Provider) CreateRelease(ctx context.Context, repo string, r provider.NewRelease) (provider.Release, error) {
	base, err := p.releasesURL(repo)
	if err != nil {
		return provider.Release{}, err
	}
	in := map[string]any{"tag_name": r.TagName, "name": r.Name, "body": r.Body, "draft": r.Draft, "prerelease": r.Prerelease}
	if r.TargetCommitish != "" {
		in["target_commitish"] = r.TargetCommitish
	}
	var out restRelease
	if err := p.c.do(ctx, http.MethodPost, base, in, &out, 0); err != nil {
		return provider.Release{}, err
	}
	return out.release(), nil
}

// assetUploadURL expands rel's upload_url template ("…/assets{?name,label}")
// for name; the host is the template's (uploads.github.com, or a fake's).
func assetUploadURL(rel provider.Release, name string) (string, error) {
	tmpl, _, _ := strings.Cut(rel.UploadURL, "{")
	u, err := url.Parse(tmpl)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("github: release %d: bad upload_url %q", rel.ID, rel.UploadURL)
	}
	u.RawQuery = url.Values{"name": {name}}.Encode()
	return u.String(), nil
}

// UploadReleaseAsset streams the file at path to rel as asset name.
func (p *Provider) UploadReleaseAsset(ctx context.Context, rel provider.Release, name, path, contentType string) (provider.ReleaseAsset, error) {
	if name == "" {
		return provider.ReleaseAsset{}, errors.New("github: upload asset: empty name")
	}
	u, err := assetUploadURL(rel, name)
	if err != nil {
		return provider.ReleaseAsset{}, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	f, err := os.Open(path) //nolint:gosec // G304: the release engine's own build output
	if err != nil {
		return provider.ReleaseAsset{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return provider.ReleaseAsset{}, err
	}
	var out restAsset
	if err := p.c.upload(ctx, u, f, st.Size(), contentType, &out); err != nil {
		return provider.ReleaseAsset{}, err
	}
	return out.asset(), nil
}

// upload POSTs body (size bytes, streamed) to u and decodes the JSON answer.
func (c *client) upload(ctx context.Context, u string, body io.Reader, size int64, contentType string, out any) error {
	if err := c.checkQuota(0); err != nil {
		return err
	}
	token, err := c.auth.AccessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, io.NopCloser(body))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	c.track(resp.Header)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if err := c.status(resp, data, req); err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: decode %s: %w", req.URL.Path, err)
	}
	return nil
}
