package control

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// WorkshopPage is POST /api/projects/{id}/steam/page.
type WorkshopPage struct {
	Item       string   `json:"item,omitempty"`
	AppID      uint32   `json:"appId,omitempty"`
	Title      string   `json:"title,omitempty"`
	LocaleDir  string   `json:"localeDir,omitempty"`
	Preview    string   `json:"preview,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Visibility string   `json:"visibility,omitempty"`
	ChangeNote string   `json:"changeNote,omitempty"`
	DryRun     bool     `json:"dryRun,omitempty"`
}

// SteamStatus reports whether the Steam client runs and is signed in (GET
// /api/steam/status), as appID or else the project's Steam target app.
func (c *Client) SteamStatus(ctx context.Context, project int64, appID uint32) (json.RawMessage, error) {
	q := url.Values{}
	if project != 0 {
		q.Set("project", strconv.FormatInt(project, 10))
	}
	if appID != 0 {
		q.Set("appId", strconv.FormatUint(uint64(appID), 10))
	}
	return c.get(ctx, "/api/steam/status", q)
}

// WorkshopItem reports the code project's recorded Workshop item.
func (c *Client) WorkshopItem(ctx context.Context, project int64) (json.RawMessage, error) {
	return c.get(ctx, idPath("/api/projects/%d/steam/item", project), nil)
}

// CreateWorkshopItem creates (once) the code project's Workshop item.
func (c *Client) CreateWorkshopItem(ctx context.Context, project int64, appID uint32, dryRun bool) (json.RawMessage, error) {
	return c.post(ctx, idPath("/api/projects/%d/steam/item", project), map[string]any{"appId": appID, "dryRun": dryRun})
}

// SetWorkshopPage writes the item's page in every language of the mod folder.
func (c *Client) SetWorkshopPage(ctx context.Context, project int64, p WorkshopPage) (json.RawMessage, error) {
	return c.post(ctx, idPath("/api/projects/%d/steam/page", project), p)
}

// addWorkshopTools registers the Steam Workshop item tools (owner server only).
func addWorkshopTools(s *mcp.Server, c *Client) {
	type create struct {
		Project int64  `json:"project" jsonschema:"the GitHub code project id from list_projects (its mapped folder holds the mod)"`
		AppID   uint32 `json:"app_id" jsonschema:"the game's Steam app id (e.g. 839770 Phoenix Point)"`
		DryRun  bool   `json:"dry_run,omitempty" jsonschema:"plan only: nothing is created"`
	}
	add(s, &mcp.Tool{Name: "create_workshop_item", Description: "Create the Steam Workshop item of a code project that has no Workshop page yet " +
		"(ISteamUGC::CreateItem through the owner's running, signed-in Steam client; no login). One item per project, ever: the new id is " +
		"recorded before anything else and a retry returns the same item; an earlier creation whose answer was lost refuses with code " +
		"create_unknown (the owner checks his Workshop files). On success the item is added to the project's publish profile as target " +
		"steam:<id> {appId} and linked to the project once the Steam sync lists it (call again to link). The item starts without content " +
		"or page: then set_workshop_page, and publish versions with release / publish_version. Public effect unless dry_run: run dry_run first." +
		" Codes: no_steam_api (steam_api64.dll not set up: no installed Steam game has one to copy into the app's tools folder), steam_refused (Steam not running / not signed in / not owning the game)." + descCallerRefusal},
		func(ctx context.Context, in create) (json.RawMessage, error) {
			return c.CreateWorkshopItem(ctx, in.Project, in.AppID, in.DryRun)
		})

	type page struct {
		Project    int64    `json:"project" jsonschema:"the GitHub code project id"`
		Item       string   `json:"item,omitempty" jsonschema:"the Workshop item id (default: the one create_workshop_item recorded, or the profile's only steam target)"`
		AppID      uint32   `json:"app_id,omitempty" jsonschema:"the game's app id (default: the profile target's appId)"`
		Title      string   `json:"title,omitempty" jsonschema:"title for every language without title.<language>.txt (default: the project name)"`
		LocaleDir  string   `json:"locale_dir,omitempty" jsonschema:"folder in the mod repo with description.<language>.txt (BBCode, Steam API language codes: english required, russian, schinese, ...) and optional title.<language>.txt; default workshop/locale"`
		Preview    string   `json:"preview,omitempty" jsonschema:"preview image in the repo (jpg/png/gif, at most 1 MB); default workshop/image/steam_preview.jpg or image/steam_preview.jpg; - keeps the current one"`
		Tags       []string `json:"tags,omitempty" jsonschema:"the item's tags (replace the current ones; omit to keep them)"`
		Visibility string   `json:"visibility,omitempty" jsonschema:"public, friends, private or unlisted (omit to keep it)"`
		ChangeNote string   `json:"change_note,omitempty" jsonschema:"change note of the update (optional)"`
		DryRun     bool     `json:"dry_run,omitempty" jsonschema:"plan only: the languages, titles, description sizes, preview, tags and visibility that would be sent"`
	}
	add(s, &mcp.Tool{Name: "set_workshop_page", Description: "Write a Steam Workshop item's page in every language found in the mod repo (one ISteamUGC update per " +
		"language with SetItemUpdateLanguage: title + description; the preview, tags and visibility once with english) through the owner's running " +
		"Steam client. Sources are read from the code project's mapped folder. Re-sending the same page is harmless. Public unless dry_run." + descCallerRefusal},
		func(ctx context.Context, in page) (json.RawMessage, error) {
			return c.SetWorkshopPage(ctx, in.Project, WorkshopPage{Item: in.Item, AppID: in.AppID, Title: in.Title, LocaleDir: in.LocaleDir,
				Preview: in.Preview, Tags: in.Tags, Visibility: in.Visibility, ChangeNote: in.ChangeNote, DryRun: in.DryRun})
		})
}
