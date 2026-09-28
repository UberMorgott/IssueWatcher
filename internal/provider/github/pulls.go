package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Publishing a fix (internal/runner Publisher): default branch, the user token
// for git push, and draft pull requests. User actions: no quota reserve.

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("github: bad repo id %q", repo)
	}
	return owner, name, nil
}

// DefaultBranch returns repo's default branch.
func (p *Provider) DefaultBranch(ctx context.Context, repo string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s", p.auth.APIURL, url.PathEscape(owner), url.PathEscape(name))
	if err := p.c.do(ctx, http.MethodGet, u, nil, &out, 0); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

// GitToken is the user access token (refreshed when due) for git over HTTPS.
func (p *Provider) GitToken(ctx context.Context) (string, error) { return p.auth.AccessToken(ctx) }

// Login is the signed-in user.
func (p *Provider) Login() string { return p.auth.Login() }

type restPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// FindPullRequest returns an existing pull request from branch head of repo
// (any state), so a retried publish never opens a second one.
func (p *Provider) FindPullRequest(ctx context.Context, repo, head string) (provider.PullRequest, bool, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return provider.PullRequest{}, false, err
	}
	var out []restPull
	q := url.Values{"head": {owner + ":" + head}, "state": {"all"}, "per_page": {"5"}}
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", p.auth.APIURL, url.PathEscape(owner), url.PathEscape(name), q.Encode())
	if err := p.c.do(ctx, http.MethodGet, u, nil, &out, 0); err != nil {
		return provider.PullRequest{}, false, err
	}
	if len(out) == 0 {
		return provider.PullRequest{}, false, nil
	}
	return provider.PullRequest{Number: out[0].Number, URL: out[0].HTMLURL}, true, nil
}

// CreatePullRequest opens a pull request (draft when pr.Draft).
func (p *Provider) CreatePullRequest(ctx context.Context, repo string, pr provider.NewPullRequest) (provider.PullRequest, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return provider.PullRequest{}, err
	}
	in := map[string]any{"title": pr.Title, "head": pr.Head, "base": pr.Base, "body": pr.Body, "draft": pr.Draft}
	var out restPull
	u := fmt.Sprintf("%s/repos/%s/%s/pulls", p.auth.APIURL, url.PathEscape(owner), url.PathEscape(name))
	if err := p.c.do(ctx, http.MethodPost, u, in, &out, 0); err != nil {
		return provider.PullRequest{}, err
	}
	return provider.PullRequest{Number: out.Number, URL: out.HTMLURL}, nil
}
