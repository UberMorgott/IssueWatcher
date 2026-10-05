package github

import (
	"context"
	"errors"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Issue actions of the autopilot release run (docs/AUTOPILOT.md → close:<item>,
// reply probe): close an issue as completed and read an issue's state with its
// latest comments. itemExternalID is the issue's GraphQL node id (as Reply).

const closeIssueMutation = `mutation($id: ID!) {
  closeIssue(input: {issueId: $id, stateReason: COMPLETED}) { issue { state } }
}`

const issueStateQuery = `query($id: ID!) {
  node(id: $id) {
    ... on Issue {
      state
      comments(last: 100) { nodes { id body url createdAt updatedAt author { login } } }
    }
  }
}`

// ErrNotIssue means the node is not an issue (or is gone).
var ErrNotIssue = errors.New("github: not an issue")

// CloseIssue closes the issue with state reason completed.
func (p *Provider) CloseIssue(ctx context.Context, itemExternalID string) error {
	var out struct {
		CloseIssue struct {
			Issue struct {
				State string `json:"state"`
			} `json:"issue"`
		} `json:"closeIssue"`
	}
	if err := p.c.graphql(ctx, closeIssueMutation, map[string]any{"id": itemExternalID}, &out, 0); err != nil {
		return err
	}
	if out.CloseIssue.Issue.State != "CLOSED" {
		return errors.New("github: the issue is not closed after closeIssue (state " + out.CloseIssue.Issue.State + ")")
	}
	return nil
}

// IssueStatus reads whether the issue is open and its latest 100 comments (oldest first).
func (p *Provider) IssueStatus(ctx context.Context, itemExternalID string) (open bool, comments []provider.Comment, err error) {
	var out struct {
		Node *struct {
			State    string `json:"state"`
			Comments struct {
				Nodes []gqlComment `json:"nodes"`
			} `json:"comments"`
		} `json:"node"`
	}
	if err := p.c.graphql(ctx, issueStateQuery, map[string]any{"id": itemExternalID}, &out, 0); err != nil {
		return false, nil, err
	}
	if out.Node == nil || out.Node.State == "" {
		return false, nil, ErrNotIssue
	}
	for _, c := range out.Node.Comments.Nodes {
		comments = append(comments, toComment(c))
	}
	return out.Node.State == "OPEN", comments, nil
}
