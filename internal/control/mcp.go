package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP page bounds: an agent's context is the budget.
const (
	mcpDefaultLimit = 20
	mcpMaxLimit     = 50
	mcpMaxLogSteps  = 200
)

func pageLimit(n int) int {
	switch {
	case n <= 0:
		return mcpDefaultLimit
	case n > mcpMaxLimit:
		return mcpMaxLimit
	}
	return n
}

// NewMCPServer exposes the control client as MCP domain tools (no raw HTTP):
// every action goes through the same endpoint and checks as the UI.
// Publishing tools (reply, push, PR) rely on the MCP client's own approval.
func NewMCPServer(c *Client, version string, log *slog.Logger) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "issuewatcher", Title: "IssueWatcher", Version: version},
		&mcp.ServerOptions{
			Logger: log,
			Instructions: "IssueWatcher tracks the user's GitHub issues and runs agent jobs (fix, reply, label) on them. " +
				"Ids are IssueWatcher's local item/job ids from list_items / list_jobs, not issue numbers. " +
				"Posting a comment, pushing or opening a PR is visible to other people: only do it when the user asked for it.",
		})
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}

	type noArgs struct{}
	add(s, &mcp.Tool{Name: "list_projects", Description: "List watched projects (repositories) with open/closed/unread counts.", Annotations: ro},
		func(ctx context.Context, _ noArgs) (json.RawMessage, error) { return c.Projects(ctx) })

	type listItems struct {
		Project int64  `json:"project,omitempty" jsonschema:"project id from list_projects"`
		State   string `json:"state,omitempty" jsonschema:"open or closed (default: both)"`
		Label   string `json:"label,omitempty" jsonschema:"only items with this label"`
		Query   string `json:"query,omitempty" jsonschema:"text search in title and body"`
		Unread  bool   `json:"unread,omitempty" jsonschema:"only unread items"`
		Limit   int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 50"`
		Cursor  string `json:"cursor,omitempty" jsonschema:"nextCursor of the previous page"`
	}
	add(s, &mcp.Tool{Name: "list_items", Description: "List issues (items), newest activity first, one page; follow nextCursor while more is true.", Annotations: ro},
		func(ctx context.Context, in listItems) (json.RawMessage, error) {
			return c.Items(ctx, ItemQuery{Project: in.Project, State: in.State, Label: in.Label, Text: in.Query, Unread: in.Unread,
				Limit: pageLimit(in.Limit), Cursor: in.Cursor})
		})

	type getItem struct {
		ID       int64 `json:"id" jsonschema:"item id"`
		Comments int   `json:"comments,omitempty" jsonschema:"comments to include, oldest first, default 20, max 50"`
	}
	add(s, &mcp.Tool{Name: "get_item", Description: "Get one issue with its body and the first comments (oldest first); " +
		"when comments.more is true, read the rest with list_item_comments from comments.nextCursor.", Annotations: ro},
		func(ctx context.Context, in getItem) (json.RawMessage, error) {
			return c.Item(ctx, in.ID, pageLimit(in.Comments))
		})

	type listComments struct {
		ID     int64  `json:"id" jsonschema:"item id"`
		Limit  int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 50"`
		Cursor string `json:"cursor,omitempty" jsonschema:"nextCursor of get_item's comments or of the previous page"`
	}
	add(s, &mcp.Tool{Name: "list_item_comments", Description: "List an issue's comments, oldest first, one page; follow nextCursor while more is true to reach the latest.", Annotations: ro},
		func(ctx context.Context, in listComments) (json.RawMessage, error) {
			return c.Comments(ctx, in.ID, pageLimit(in.Limit), in.Cursor)
		})

	type listJobs struct {
		State   string `json:"state,omitempty" jsonschema:"queued, running, needs_review, done, failed or cancelled"`
		Flow    string `json:"flow,omitempty" jsonschema:"fix, reply or label"`
		Origin  string `json:"origin,omitempty" jsonschema:"manual or rule"`
		Project int64  `json:"project,omitempty" jsonschema:"project id"`
		Item    int64  `json:"item,omitempty" jsonschema:"item id"`
		Limit   int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 50"`
		Cursor  string `json:"cursor,omitempty" jsonschema:"nextCursor of the previous page"`
	}
	add(s, &mcp.Tool{Name: "list_jobs", Description: "List agent jobs, newest first, one page.", Annotations: ro},
		func(ctx context.Context, in listJobs) (json.RawMessage, error) {
			return c.Jobs(ctx, JobQuery{State: in.State, Flow: in.Flow, Origin: in.Origin, Project: in.Project, Item: in.Item,
				Limit: pageLimit(in.Limit), Cursor: in.Cursor})
		})

	type jobID struct {
		ID int64 `json:"id" jsonschema:"job id"`
	}
	add(s, &mcp.Tool{Name: "get_job", Description: "Get one agent job: state, phase, result (reply draft, labels, local fix outcome, PR).", Annotations: ro},
		func(ctx context.Context, in jobID) (json.RawMessage, error) { return c.Job(ctx, in.ID) })

	type jobLog struct {
		ID      int64 `json:"id" jsonschema:"job id"`
		Attempt int   `json:"attempt,omitempty" jsonschema:"attempt number (default: the current one)"`
		Tail    int   `json:"tail,omitempty" jsonschema:"last steps to return, default 50, max 200"`
	}
	add(s, &mcp.Tool{Name: "get_job_log", Description: "Get the last steps of a job attempt's agent log.", Annotations: ro},
		func(ctx context.Context, in jobLog) (json.RawMessage, error) {
			raw, err := c.JobLog(ctx, in.ID, in.Attempt)
			if err != nil {
				return nil, err
			}
			return tailLog(raw, in.Tail)
		})

	add(s, &mcp.Tool{Name: "sync_now", Description: "Ask IssueWatcher to sync all projects now (returns at once)."},
		func(ctx context.Context, _ noArgs) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), c.Sync(ctx)
		})

	type replyItem struct {
		ID   int64  `json:"id" jsonschema:"item id"`
		Body string `json:"body" jsonschema:"comment text (Markdown), posted publicly as the user"`
	}
	add(s, &mcp.Tool{Name: "reply_item", Description: "Post a comment on the issue on its platform (GitHub) as the signed-in user. Public."},
		func(ctx context.Context, in replyItem) (json.RawMessage, error) { return c.Reply(ctx, in.ID, in.Body) })

	type startJobs struct {
		Flow      string  `json:"flow" jsonschema:"fix, reply or label"`
		ItemIDs   []int64 `json:"item_ids" jsonschema:"item ids, at most 50"`
		ProfileID string  `json:"profile_id,omitempty" jsonschema:"agent profile id (default: the flow's role)"`
	}
	add(s, &mcp.Tool{Name: "start_jobs", Description: "Queue one agent job per item. fix edits the mapped local folder and commits; reply drafts a comment; label suggests labels. Nothing is published without a later action."},
		func(ctx context.Context, in startJobs) (json.RawMessage, error) {
			if len(in.ItemIDs) == 0 || len(in.ItemIDs) > mcpMaxLimit {
				return nil, fmt.Errorf("item_ids: give 1 to %d ids", mcpMaxLimit)
			}
			return c.CreateJobs(ctx, in.Flow, in.ItemIDs, in.ProfileID)
		})

	for name, action := range map[string]struct{ action, desc string }{
		"cancel_job": {"cancel", "Cancel a queued/running job, or reject one that needs review."},
		"retry_job":  {"retry", "Run a failed/cancelled/needs_review job again as its next attempt."},
		"push_job":   {"push", "Push a direct fix job's local commits to the project's branch on the platform. Public."},
		"create_pr":  {"pr", "Publish a worktree fix job as a draft pull request. Public."},
	} {
		add(s, &mcp.Tool{Name: name, Description: action.desc},
			func(ctx context.Context, in jobID) (json.RawMessage, error) {
				return c.JobAction(ctx, in.ID, action.action)
			})
	}

	type jobReply struct {
		ID   int64  `json:"id" jsonschema:"reply job id"`
		Body string `json:"body" jsonschema:"final comment text (the job's draft, edited as needed); posted publicly"`
	}
	add(s, &mcp.Tool{Name: "send_job_reply", Description: "Post a reply job's (edited) draft as a comment on its issue. Public."},
		func(ctx context.Context, in jobReply) (json.RawMessage, error) {
			return c.JobReply(ctx, in.ID, in.Body)
		})

	type jobLabels struct {
		ID     int64    `json:"id" jsonschema:"label job id"`
		Labels []string `json:"labels" jsonschema:"existing repository label names to add (never removes)"`
	}
	add(s, &mcp.Tool{Name: "apply_job_labels", Description: "Add labels to a label job's issue on the platform (checked against the repo's labels, add only)."},
		func(ctx context.Context, in jobLabels) (json.RawMessage, error) {
			return c.JobLabels(ctx, in.ID, in.Labels)
		})
	return s
}

// add registers a tool whose handler returns the API's JSON as text content.
func add[In any](s *mcp.Server, t *mcp.Tool, h func(context.Context, In) (json.RawMessage, error)) {
	mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := h(ctx, in)
		if err != nil {
			return nil, nil, err // → isError result with the API's message
		}
		if len(out) == 0 {
			out = json.RawMessage(`{"ok":true}`)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil, nil
	})
}

// tailLog keeps the last n steps of a {attempt, steps} log.
func tailLog(raw json.RawMessage, n int) (json.RawMessage, error) {
	if n <= 0 {
		n = 50
	}
	n = min(n, mcpMaxLogSteps)
	var l struct {
		Attempt int               `json:"attempt"`
		Steps   []json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("job log: %w", err)
	}
	total := len(l.Steps)
	if total > n {
		l.Steps = l.Steps[total-n:]
	}
	return json.Marshal(map[string]any{"attempt": l.Attempt, "total": total, "steps": l.Steps})
}
