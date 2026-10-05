package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Autopilot release runs (docs/AUTOPILOT.md → MCP / CLI surface): the
// publish profile, release plans and runs, the kill switch and the activity
// log, through the same endpoints as the UI.

// ReleaseRequest is the body of POST /api/projects/{id}/release[/plan]
// (release.Request); zero fields = the profile's defaults.
type ReleaseRequest struct {
	Version string   `json:"version,omitempty"`
	Head    string   `json:"head,omitempty"`
	Items   []int64  `json:"items,omitempty"`
	Targets []string `json:"targets,omitempty"`
	DryRun  bool     `json:"dryRun,omitempty"`
}

// RunQuery are the GET /api/runs filters (zero = unset).
type RunQuery struct {
	Project int64
	Kind    string
	State   string
	Limit   int
}

// PublishProfile returns the project's ProfileDoc: publish profile, autopilot
// switches, global caps and the resolved dry-run plan.
func (c *Client) PublishProfile(ctx context.Context, project int64) (json.RawMessage, error) {
	return c.get(ctx, idPath("/api/projects/%d/publish-profile", project), nil)
}

// SetPublishProfile PUTs {revision?, publishProfile?, autopilot?}: each given
// block replaces the stored one. Refused (403 agent_caller) for agent runs.
func (c *Client) SetPublishProfile(ctx context.Context, project int64, body json.RawMessage) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return nil, errors.New("control: publish profile body must be a JSON object {revision?, publishProfile?, autopilot?}")
	}
	return c.Do(ctx, http.MethodPut, idPath("/api/projects/%d/publish-profile", project), nil, body)
}

// ReleasePlan returns the dry-run plan (ok, refusals, version, steps, targets, caps).
func (c *Client) ReleasePlan(ctx context.Context, project int64, req ReleaseRequest) (json.RawMessage, error) {
	return c.post(ctx, idPath("/api/projects/%d/release/plan", project), req)
}

// Release starts a release run (202 {run, plan}) or, with DryRun, returns the
// plan. A refusal is an *APIError (409) with Code and Refusals.
func (c *Client) Release(ctx context.Context, project int64, req ReleaseRequest) (json.RawMessage, error) {
	return c.post(ctx, idPath("/api/projects/%d/release", project), req)
}

// Runs lists autopilot runs, newest first.
func (c *Client) Runs(ctx context.Context, f RunQuery) (json.RawMessage, error) {
	q := url.Values{}
	setInt(q, "project", f.Project)
	set(q, "kind", f.Kind)
	set(q, "state", f.State)
	setInt(q, "limit", int64(f.Limit))
	return c.get(ctx, "/api/runs", q)
}

// Run returns one run's RunView {run, steps, items}.
func (c *Client) Run(ctx context.Context, id int64) (json.RawMessage, error) {
	return c.get(ctx, idPath("/api/runs/%d", id), nil)
}

// RunActions are the POST /api/runs/{id}/<action> endpoints without a body.
var RunActions = []string{"resume", "cancel"}

// RunAction runs one of RunActions.
func (c *Client) RunAction(ctx context.Context, id int64, action string) (json.RawMessage, error) {
	return c.post(ctx, fmt.Sprintf("/api/runs/%d/%s", id, url.PathEscape(action)), nil)
}

// SkipStep skips a run's publish / available step of a target; step may be
// "publish:<target>" with target "".
func (c *Client) SkipStep(ctx context.Context, id int64, step, target string) (json.RawMessage, error) {
	return c.post(ctx, idPath("/api/runs/%d/skip", id), map[string]string{"step": step, "target": target})
}

// PauseAutopilot sets (paused) or clears the kill switch: project > 0 turns
// that project's autopilot off / on, 0 the global pause.
func (c *Client) PauseAutopilot(ctx context.Context, project int64, paused bool) (json.RawMessage, error) {
	body := map[string]any{"paused": paused}
	if project > 0 {
		body["project"] = project
	}
	return c.post(ctx, "/api/autopilot/pause", body)
}

// AutopilotEvents returns {events, unread, attention} of the activity log.
func (c *Client) AutopilotEvents(ctx context.Context, unreadOnly bool, limit int) (json.RawMessage, error) {
	q := url.Values{}
	if unreadOnly {
		q.Set("unreadOnly", "true")
	}
	setInt(q, "limit", int64(limit))
	return c.get(ctx, "/api/autopilot/events", q)
}

// runPoll is how often RunWait reads the run.
var runPoll = 2 * time.Second

// RunWait polls run id until it is no longer pending or running (done,
// cancelled, failed or held) or ctx ends (the run goes on in the app).
func (c *Client) RunWait(ctx context.Context, id int64) (json.RawMessage, error) {
	for {
		view, err := c.Run(ctx, id)
		if err != nil {
			return view, err
		}
		var v struct {
			Run struct {
				State string `json:"state"`
			} `json:"run"`
		}
		if err := json.Unmarshal(view, &v); err != nil {
			return view, fmt.Errorf("run %d: %w", id, err)
		}
		if v.Run.State != "pending" && v.Run.State != "running" {
			return view, nil
		}
		select {
		case <-ctx.Done():
			return view, ctx.Err()
		case <-time.After(runPoll):
		}
	}
}

// ResolveProject turns a project id ("12") or key into its id: the settings
// key ("github:owner/repo") or the name ("owner/repo"), the GitHub code
// project first when a name matches several.
func (c *Client) ResolveProject(ctx context.Context, s string) (int64, error) {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		if id <= 0 {
			return 0, fmt.Errorf("control: bad project id %q", s)
		}
		return id, nil
	}
	if s == "" {
		return 0, errors.New("control: empty project")
	}
	raw, err := c.Projects(ctx)
	if err != nil {
		return 0, err
	}
	var ps []struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
		Key      string `json:"key"`
	}
	if err := json.Unmarshal(raw, &ps); err != nil {
		return 0, fmt.Errorf("control: projects: %w", err)
	}
	var byName, code []int64
	for _, p := range ps {
		if strings.EqualFold(p.Key, s) {
			return p.ID, nil
		}
		if strings.EqualFold(p.Name, s) {
			byName = append(byName, p.ID)
			if p.Platform == "github" {
				code = append(code, p.ID)
			}
		}
	}
	switch {
	case len(byName) == 1:
		return byName[0], nil
	case len(code) == 1:
		return code[0], nil
	case len(byName) > 1:
		return 0, fmt.Errorf("control: project %q is ambiguous (ids %v): give the id or the key", s, byName)
	}
	return 0, fmt.Errorf("control: no project %q (give an id, key or name from projects)", s)
}
