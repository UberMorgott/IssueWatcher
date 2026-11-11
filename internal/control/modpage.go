package control

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ModPageEdit is the PUT /api/projects/{id}/page body (provider.ModPageEdit):
// nil fields stay as they are on the page.
type ModPageEdit struct {
	Name        *string `json:"name,omitempty"`
	Summary     *string `json:"summary,omitempty"`
	Description *string `json:"description,omitempty"`
	Version     *string `json:"version,omitempty"`
	DryRun      bool    `json:"dryRun,omitempty"`
}

// ModPage reads a mod project's page (name, summary, BBCode description, version, ...).
func (c *Client) ModPage(ctx context.Context, project int64) (json.RawMessage, error) {
	return c.get(ctx, idPath("/api/projects/%d/page", project), nil)
}

// SaveModPage writes the given page fields (or plans it with DryRun).
func (c *Client) SaveModPage(ctx context.Context, project int64, edit ModPageEdit) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPut, idPath("/api/projects/%d/page", project), nil, edit)
}

// addModPageTools registers the mod page tools (owner server only).
func addModPageTools(s *mcp.Server, c *Client, ro *mcp.ToolAnnotations) {
	type get struct {
		Project int64 `json:"project" jsonschema:"the mod project id from list_projects (Nexus)"`
	}
	add(s, &mcp.Tool{Name: "get_mod_page", Description: "Read a mod project's page as the site's mod editor loads it (Nexus): name, summary, " +
		"description (BBCode), version, category, author, tags, url, maxSummary. Read-only.", Annotations: ro},
		func(ctx context.Context, in get) (json.RawMessage, error) { return c.ModPage(ctx, in.Project) })

	type set struct {
		Project     int64   `json:"project" jsonschema:"the mod project id from list_projects (Nexus)"`
		Name        *string `json:"name,omitempty" jsonschema:"new mod name; omitted = unchanged"`
		Summary     *string `json:"summary,omitempty" jsonschema:"new summary (at most maxSummary chars); omitted = unchanged"`
		Description *string `json:"description,omitempty" jsonschema:"new description (BBCode, the whole text); omitted = unchanged"`
		Version     *string `json:"version,omitempty" jsonschema:"new mod version; omitted = unchanged"`
		DryRun      bool    `json:"dry_run,omitempty" jsonschema:"plan only: changed fields and the request, nothing sent"`
	}
	add(s, &mcp.Tool{Name: "set_mod_page", Description: "Save a mod project's page fields on its platform (Nexus mod editor through the signed-in " +
		"session); omitted fields keep the page's current value. Public unless dry_run: run dry_run first. Code save_unsure: sent without a " +
		"clear answer, check get_mod_page before trying again." + descCallerRefusal},
		func(ctx context.Context, in set) (json.RawMessage, error) {
			return c.SaveModPage(ctx, in.Project, ModPageEdit{Name: in.Name, Summary: in.Summary, Description: in.Description, Version: in.Version, DryRun: in.DryRun})
		})
}
