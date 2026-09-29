package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

var _ provider.Labeler = (*Provider)(nil)

// ListLabels implements provider.Labeler (GET /repos/{o}/{r}/labels, all pages).
func (p *Provider) ListLabels(ctx context.Context, repo string) ([]provider.Label, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	out := []provider.Label{}
	for page := 1; ; page++ {
		var chunk []struct {
			Name        string `json:"name"`
			Color       string `json:"color"`
			Description string `json:"description"`
		}
		u := fmt.Sprintf("%s/repos/%s/%s/labels?per_page=100&page=%d", p.auth.APIURL, url.PathEscape(owner), url.PathEscape(name), page)
		if err := p.c.do(ctx, http.MethodGet, u, nil, &chunk, 0); err != nil {
			return nil, err
		}
		for _, l := range chunk {
			out = append(out, provider.Label{Name: l.Name, Color: l.Color, Description: l.Description})
		}
		if len(chunk) < 100 {
			return out, nil
		}
	}
}

// AddLabels implements provider.Labeler: POST /repos/{o}/{r}/issues/{n}/labels
// adds to the issue's labels (it never removes any) and answers with all of them.
// GitHub creates names that do not exist yet, so callers pass checked names only.
func (p *Provider) AddLabels(ctx context.Context, repo string, number int, names []string) ([]string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var out []struct {
		Name string `json:"name"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", p.auth.APIURL, url.PathEscape(owner), url.PathEscape(name), number)
	if err := p.c.do(ctx, http.MethodPost, u, map[string]any{"labels": names}, &out, 0); err != nil {
		return nil, err
	}
	labels := make([]string, 0, len(out))
	for _, l := range out {
		labels = append(labels, l.Name)
	}
	return labels, nil
}
