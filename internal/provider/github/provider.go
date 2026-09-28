package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Provider implements provider.Provider for github.com.
type Provider struct {
	auth *Auth
	c    *client
}

var _ provider.Provider = (*Provider)(nil)

// NewProvider uses auth for credentials and endpoints.
func NewProvider(auth *Auth) *Provider {
	return &Provider{auth: auth, c: &client{auth: auth, http: auth.HTTP, now: auth.Now}}
}

// Platform implements provider.Provider.
func (p *Provider) Platform() string { return "github" }

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		ListProjects: true, SyncItems: true, ListComments: true, Reply: true, CreatePR: true,
		Auth: provider.AuthOAuthLoopback,
	}
}

// RateStatus implements provider.RateReporter.
func (p *Provider) RateStatus() (provider.RateStatus, bool) { return p.c.rate() }

// Account implements provider.Provider.
func (p *Provider) Account(ctx context.Context) (string, error) {
	if _, err := p.auth.AccessToken(ctx); err != nil {
		return "", err
	}
	return p.auth.Login(), nil
}

// ListProjects returns every repo the user reaches through the app's installations.
func (p *Provider) ListProjects(ctx context.Context) ([]provider.Project, error) {
	var installs []int64
	for page := 1; ; page++ {
		var out struct {
			Installations []struct {
				ID int64 `json:"id"`
			} `json:"installations"`
		}
		u := fmt.Sprintf("%s/user/installations?per_page=100&page=%d", p.auth.APIURL, page)
		if err := p.c.do(ctx, http.MethodGet, u, nil, &out, minRemaining); err != nil {
			return nil, err
		}
		for _, in := range out.Installations {
			installs = append(installs, in.ID)
		}
		if len(out.Installations) < 100 {
			break
		}
	}
	seen := map[string]bool{}
	var projects []provider.Project
	for _, id := range installs {
		for page := 1; ; page++ {
			var out struct {
				Repositories []struct {
					FullName string `json:"full_name"`
					HTMLURL  string `json:"html_url"`
				} `json:"repositories"`
			}
			u := fmt.Sprintf("%s/user/installations/%d/repositories?per_page=100&page=%d", p.auth.APIURL, id, page)
			if err := p.c.do(ctx, http.MethodGet, u, nil, &out, minRemaining); err != nil {
				return nil, err
			}
			for _, r := range out.Repositories {
				if !seen[r.FullName] {
					seen[r.FullName] = true
					projects = append(projects, provider.Project{ExternalID: r.FullName, Name: r.FullName, URL: r.HTMLURL})
				}
			}
			if len(out.Repositories) < 100 {
				break
			}
		}
	}
	return projects, nil
}

const commentFields = `pageInfo { hasNextPage endCursor }
      nodes { id body url createdAt updatedAt author { login } }`

// issueFields is one issue with labels and its first comment page.
const issueFields = `id number title body url state stateReason createdAt updatedAt closedAt
        author { login }
        labels(first: 50) { nodes { name } }
        comments(first: 100) { ` + commentFields + ` }`

// issuesQuery pages issues (pull requests are a separate connection, so none
// appear here) updated since $since, oldest update first.
const issuesQuery = `query($owner: String!, $name: String!, $since: DateTime, $after: String) {
  repository(owner: $owner, name: $name) {
    issues(first: 50, after: $after, orderBy: {field: UPDATED_AT, direction: ASC}, filterBy: {since: $since}) {
      pageInfo { hasNextPage endCursor }
      nodes { ` + issueFields + ` }
    }
  }
}`

const moreCommentsQuery = `query($id: ID!, $after: String) {
  node(id: $id) { ... on Issue { comments(first: 100, after: $after) { ` + commentFields + ` } } }
}`

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type actor struct {
	Login string `json:"login"`
}

type gqlComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Author    *actor    `json:"author"`
}

type commentConn struct {
	PageInfo pageInfo     `json:"pageInfo"`
	Nodes    []gqlComment `json:"nodes"`
}

type gqlIssue struct {
	ID          string     `json:"id"`
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	URL         string     `json:"url"`
	State       string     `json:"state"`
	StateReason string     `json:"stateReason"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ClosedAt    *time.Time `json:"closedAt"`
	Author      *actor     `json:"author"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Comments commentConn `json:"comments"`
}

func login(a *actor) string {
	if a == nil {
		return "ghost" // deleted account
	}
	return a.Login
}

// SyncItems implements provider.Provider.
func (p *Provider) SyncItems(ctx context.Context, project provider.Project, since time.Time) ([]provider.Item, error) {
	owner, name, ok := strings.Cut(project.ExternalID, "/")
	if !ok {
		return nil, fmt.Errorf("github: bad repo id %q", project.ExternalID)
	}
	vars := map[string]any{"owner": owner, "name": name, "since": nil, "after": nil}
	if !since.IsZero() {
		vars["since"] = since.UTC().Format(time.RFC3339)
	}
	var items []provider.Item
	for {
		var out struct {
			Repository *struct {
				Issues struct {
					PageInfo pageInfo   `json:"pageInfo"`
					Nodes    []gqlIssue `json:"nodes"`
				} `json:"issues"`
			} `json:"repository"`
		}
		if err := p.c.graphql(ctx, issuesQuery, vars, &out, minRemaining); err != nil {
			return nil, err
		}
		if out.Repository == nil {
			return nil, fmt.Errorf("github: repository %s not accessible", project.ExternalID)
		}
		for i := range out.Repository.Issues.Nodes {
			it, err := p.toItem(ctx, &out.Repository.Issues.Nodes[i])
			if err != nil {
				return nil, err
			}
			items = append(items, it)
		}
		pi := out.Repository.Issues.PageInfo
		if !pi.HasNextPage {
			return items, nil
		}
		vars["after"] = pi.EndCursor
	}
}

func (p *Provider) toItem(ctx context.Context, n *gqlIssue) (provider.Item, error) {
	it := provider.Item{
		ExternalID: n.ID, Kind: "issue", Number: n.Number, Title: n.Title, Body: n.Body, URL: n.URL,
		Author: login(n.Author), Open: n.State == "OPEN", RawStatus: strings.ToLower(n.State),
		CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
	}
	if n.StateReason != "" {
		it.RawStatus += ":" + strings.ToLower(n.StateReason)
	}
	if n.ClosedAt != nil && !it.Open {
		it.ClosedAt = *n.ClosedAt
	}
	for _, l := range n.Labels.Nodes {
		it.Labels = append(it.Labels, l.Name)
	}
	conn := n.Comments
	for {
		for _, c := range conn.Nodes {
			it.Comments = append(it.Comments, toComment(c))
		}
		if !conn.PageInfo.HasNextPage {
			return it, nil
		}
		var out struct {
			Node *struct {
				Comments commentConn `json:"comments"`
			} `json:"node"`
		}
		vars := map[string]any{"id": n.ID, "after": conn.PageInfo.EndCursor}
		if err := p.c.graphql(ctx, moreCommentsQuery, vars, &out, minRemaining); err != nil {
			return it, err
		}
		if out.Node == nil {
			return it, nil
		}
		conn = out.Node.Comments
	}
}

func toComment(c gqlComment) provider.Comment {
	return provider.Comment{
		ExternalID: c.ID, Author: login(c.Author), Body: c.Body, URL: c.URL,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

const addCommentMutation = `mutation($id: ID!, $body: String!) {
  addComment(input: {subjectId: $id, body: $body}) {
    commentEdge { node { id body url createdAt updatedAt author { login } } }
  }
}`

// Reply implements provider.Provider.
func (p *Provider) Reply(ctx context.Context, itemExternalID, body string) (provider.Comment, error) {
	var out struct {
		AddComment struct {
			CommentEdge struct {
				Node gqlComment `json:"node"`
			} `json:"commentEdge"`
		} `json:"addComment"`
	}
	vars := map[string]any{"id": itemExternalID, "body": body}
	if err := p.c.graphql(ctx, addCommentMutation, vars, &out, 0); err != nil {
		return provider.Comment{}, err
	}
	return toComment(out.AddComment.CommentEdge.Node), nil
}
