package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func changelogPath(project int64, version string) string {
	return fmt.Sprintf("/api/projects/%d/changelogs/%s", project, url.PathEscape(version))
}

// Changelogs lists a mod project's changelog versions with their entries.
func (c *Client) Changelogs(ctx context.Context, project int64) (json.RawMessage, error) {
	return c.get(ctx, idPath("/api/projects/%d/changelogs", project), nil)
}

// CheckChangelogs compares the editor's changelog with the public API (Nexus v1, stored API key).
func (c *Client) CheckChangelogs(ctx context.Context, project int64) (json.RawMessage, error) {
	return c.get(ctx, idPath("/api/projects/%d/changelogs/check", project), nil)
}

// SetChangelog replaces every entry of version with lines (adds the version when absent).
func (c *Client) SetChangelog(ctx context.Context, project int64, version string, lines []string, dryRun bool) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPut, changelogPath(project, version), nil, map[string]any{"lines": lines, "dryRun": dryRun})
}

// DeleteChangelog deletes every entry of version.
func (c *Client) DeleteChangelog(ctx context.Context, project int64, version string, dryRun bool) (json.RawMessage, error) {
	var q url.Values
	if dryRun {
		q = url.Values{"dryRun": {"1"}}
	}
	return c.Do(ctx, http.MethodDelete, changelogPath(project, version), q, nil)
}

// addChangelogTools registers the mod changelog tools (owner server only).
func addChangelogTools(s *mcp.Server, c *Client, ro *mcp.ToolAnnotations) {
	type list struct {
		Project int64 `json:"project" jsonschema:"the mod project id from list_projects (Nexus)"`
	}
	add(s, &mcp.Tool{Name: "list_mod_changelogs", Description: "List a mod project's changelog as the site's mod editor loads it (Nexus): versions newest first, " +
		"each with its entries {id, text}. Read-only.", Annotations: ro},
		func(ctx context.Context, in list) (json.RawMessage, error) { return c.Changelogs(ctx, in.Project) })
	add(s, &mcp.Tool{Name: "check_mod_changelogs", Description: "Compare a mod project's changelog in the site's editor with what the public API serves " +
		"(Nexus v1 changelogs.json, read with the API key from Settings › Платформы › Nexus): per version expected vs public line counts, match, " +
		"missing/extra lines. The public API may serve a cached copy for a few minutes after a change. Read-only.", Annotations: ro},
		func(ctx context.Context, in list) (json.RawMessage, error) { return c.CheckChangelogs(ctx, in.Project) })

	type set struct {
		Project int64    `json:"project" jsonschema:"the mod project id from list_projects (Nexus)"`
		Version string   `json:"version" jsonschema:"the mod version whose changelog is replaced (e.g. 0.2.4)"`
		Lines   []string `json:"lines" jsonschema:"the new entries, one per line (a leading '- ' is dropped; empty lines skipped)"`
		DryRun  bool     `json:"dry_run,omitempty" jsonschema:"plan only: the action, before/after lines and the request, nothing sent"`
	}
	add(s, &mcp.Tool{Name: "set_mod_changelog", Description: "Replace a mod version's changelog on its platform (Nexus mod editor through the signed-in session): every " +
		"current entry of the version is replaced by lines in one request (the version is added when it has none; identical lines send nothing), so " +
		"no duplicate entries. Other versions are never touched. After a real change the result's check re-reads the public API per version " +
		"(may be cached: a mismatch is a note, not a failure). Public unless dry_run: run dry_run first. Code save_unsure: sent without a clear " +
		"answer, check list_mod_changelogs before trying again." + descCallerRefusal},
		func(ctx context.Context, in set) (json.RawMessage, error) {
			return c.SetChangelog(ctx, in.Project, in.Version, in.Lines, in.DryRun)
		})

	type del struct {
		Project int64  `json:"project" jsonschema:"the mod project id from list_projects (Nexus)"`
		Version string `json:"version" jsonschema:"the mod version whose changelog entries are all deleted"`
		DryRun  bool   `json:"dry_run,omitempty" jsonschema:"plan only: nothing sent"`
	}
	add(s, &mcp.Tool{Name: "delete_mod_changelog", Description: "Delete every changelog entry of one mod version on its platform (Nexus mod editor, " +
		"through the signed-in session); a version without entries sends nothing. Cannot be undone; public unless dry_run." + descCallerRefusal},
		func(ctx context.Context, in del) (json.RawMessage, error) {
			return c.DeleteChangelog(ctx, in.Project, in.Version, in.DryRun)
		})
}
