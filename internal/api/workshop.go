package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider/steamugc"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// WorkshopItems creates Steam Workshop items and writes their pages
// (steamugc.Client).
type WorkshopItems interface {
	Item(key string) (uint64, bool, error)
	Create(ctx context.Context, key string, appID uint32, dryRun bool) (steamugc.Created, error)
	ResetCreate(key string) error
	SetPage(ctx context.Context, appID uint32, item uint64, p steamugc.Page, changeNote string, dryRun bool) (steamugc.PageResult, error)
}

// New Workshop items and their pages in every language, for a code project
// (its mapped folder holds the page sources). Writes are refused for agent
// runs unless dryRun.
//
//	GET    /api/projects/{id}/steam/item          {item, creating, url?}
//	POST   /api/projects/{id}/steam/item          {appId, dryRun?} → {item, url, created, linked, target, needsLegalAgreement?}; 409 {code: create_unknown}
//	DELETE /api/projects/{id}/steam/item/pending  drop an unanswered creation (after checking the Workshop files)
//	POST   /api/projects/{id}/steam/page          {item?, appId?, title?, localeDir?, preview?, tags?, visibility?, changeNote?, dryRun?} → steamugc.PageResult
func (s *Server) registerWorkshop(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{id}/steam/item", s.handleWorkshopItem)
	mux.HandleFunc("POST /api/projects/{id}/steam/item", s.handleWorkshopCreate)
	mux.HandleFunc("DELETE /api/projects/{id}/steam/item/pending", s.handleWorkshopReset)
	mux.HandleFunc("POST /api/projects/{id}/steam/page", s.handleWorkshopPage)
}

func (s *Server) workshopError(w http.ResponseWriter, err error) {
	code, status := "steam_error", http.StatusBadGateway
	switch {
	case errors.Is(err, steamugc.ErrBadPage):
		code, status = "bad_request", http.StatusBadRequest
	case errors.Is(err, steamugc.ErrCreateUnknown):
		code, status = "create_unknown", http.StatusConflict
	case errors.Is(err, steamugc.ErrNoSteamAPI):
		code, status = "no_steam_api", http.StatusConflict
	case errors.Is(err, steamugc.ErrSteam):
		code, status = "steam_refused", http.StatusConflict
	}
	if status == http.StatusBadGateway {
		s.opts.Log.Warn("api: workshop", "err", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
}

func (s *Server) handleWorkshopItem(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	item, creating, err := s.opts.Workshop.Item(rp.Key)
	if err != nil {
		s.internalError(w, "workshop item", err)
		return
	}
	out := map[string]any{"item": item, "creating": creating}
	if item != 0 {
		out["url"] = steamugc.ItemURL(item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleWorkshopCreate(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	var req struct {
		AppID  uint32 `json:"appId"`
		DryRun bool   `json:"dryRun"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.AppID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "appId: the game's Steam app id", "code": "bad_request"})
		return
	}
	if !req.DryRun && s.refuseAgent(w, r) {
		return
	}
	c, err := s.opts.Workshop.Create(r.Context(), rp.Key, req.AppID, req.DryRun)
	if err != nil {
		s.workshopError(w, err)
		return
	}
	out := map[string]any{"item": c.Item, "url": c.URL, "created": c.Created, "dryRun": c.DryRun, "plan": c.Plan,
		"needsLegalAgreement": c.NeedsLegal, "linked": false}
	if c.DryRun {
		out["target"] = "steam:<new id> {appId: " + strconv.FormatUint(uint64(req.AppID), 10) + "} in the publish profile, then linked to " + rp.Key
		writeJSON(w, http.StatusOK, out)
		return
	}
	target := "steam:" + strconv.FormatUint(c.Item, 10)
	out["target"] = target
	if err := s.addSteamTarget(rp.Key, target, req.AppID); err != nil {
		out["targetError"] = err.Error()
	}
	linked, err := s.linkSteamItem(r.Context(), rp, target)
	out["linked"] = linked
	if err != nil {
		out["linkError"] = err.Error()
	}
	if !linked && s.opts.Sync != nil {
		s.opts.Sync.Trigger() // the item shows up after the next Steam sync; call again to link it
		out["linkNote"] = "the item is linked once the Steam sync lists it: call create again (it reuses the item)"
	}
	s.opts.Log.Info("workshop item", "project", rp.Key, "item", c.Item, "created", c.Created, "linked", linked)
	writeJSON(w, http.StatusOK, out)
}

// addSteamTarget puts target (appId) into the project's publish profile unless present.
func (s *Server) addSteamTarget(key, target string, appID uint32) error {
	if s.opts.Settings == nil {
		return errors.New("no settings store")
	}
	for range 3 {
		cur, err := s.opts.Settings.Settings()
		if err != nil {
			return err
		}
		if _, ok := cur.Settings.Agents.Projects[key].PublishProfile.Targets[target]; ok {
			return nil
		}
		patch, _ := json.Marshal(map[string]any{"agents": map[string]any{"projects": map[string]any{key: map[string]any{
			"publishProfile": map[string]any{"targets": map[string]any{target: map[string]any{"appId": appID}}}}}}})
		doc, err := s.opts.Settings.PatchSettings(cur.Revision, patch)
		if err == nil {
			s.Publish(EventSettingsChanged, doc)
			return nil
		}
		if !errors.Is(err, config.ErrConflict) {
			return err
		}
	}
	return errors.New("settings kept changing: add the target in the publish profile")
}

// linkSteamItem links the synced mod page of target to the code project.
func (s *Server) linkSteamItem(ctx context.Context, rp store.Repo, target string) (bool, error) {
	repos, err := s.opts.Store.Repos(ctx)
	if err != nil {
		return false, err
	}
	var page *store.Repo
	for i := range repos {
		if repos[i].Key == target {
			page = &repos[i]
		}
	}
	if page == nil {
		return false, nil
	}
	ids := []int64{}
	for _, l := range rp.Links {
		if l == page.ID {
			return true, nil
		}
		ids = append(ids, l)
	}
	if err := s.opts.Store.SetProjectLinks(ctx, rp.ID, append(ids, page.ID)); err != nil {
		return false, fmt.Errorf("link %s: %w", target, err)
	}
	return true, nil
}

func (s *Server) handleWorkshopReset(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok || s.refuseAgent(w, r) {
		return
	}
	if err := s.opts.Workshop.ResetCreate(rp.Key); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "has_item"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWorkshopPage(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.codeProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Item       string   `json:"item"`
		AppID      uint32   `json:"appId"`
		Title      string   `json:"title"`
		LocaleDir  string   `json:"localeDir"`
		Preview    string   `json:"preview"`
		Tags       []string `json:"tags"`
		Visibility string   `json:"visibility"`
		ChangeNote string   `json:"changeNote"`
		DryRun     bool     `json:"dryRun"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !req.DryRun && s.refuseAgent(w, r) {
		return
	}
	item, err := strconv.ParseUint(strings.TrimPrefix(req.Item, "steam:"), 10, 64)
	if req.Item == "" {
		item, _, err = s.opts.Workshop.Item(rp.Key)
		if err == nil && item == 0 {
			item, err = s.singleSteamTarget(rp.Key)
		}
	}
	if err != nil || item == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "item: the Workshop item id (none created or configured for this project)", "code": "bad_request"})
		return
	}
	app := req.AppID
	if app == 0 && s.opts.Settings != nil {
		if cur, err := s.opts.Settings.Settings(); err == nil {
			app = uint32(max(0, min(cur.Settings.Agents.Projects[rp.Key].PublishProfile.Targets["steam:"+strconv.FormatUint(item, 10)].AppID, 1<<31)))
		}
	}
	if app == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "appId: the game's Steam app id (not in the publish profile)", "code": "bad_request"})
		return
	}
	title := req.Title
	if title == "" {
		title = rp.Name
	}
	page, err := steamugc.LoadPage(rp.LocalPath, steamugc.PageSpec{LocaleDir: req.LocaleDir, Title: title, Preview: req.Preview, Tags: req.Tags, Visibility: req.Visibility})
	if err != nil {
		s.workshopError(w, err)
		return
	}
	res, err := s.opts.Workshop.SetPage(r.Context(), app, item, page, req.ChangeNote, req.DryRun)
	if err != nil {
		if len(res.Languages) > 0 {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "code": "steam_refused", "result": res})
			return
		}
		s.workshopError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// singleSteamTarget is the only steam:<id> target of the project's publish profile.
func (s *Server) singleSteamTarget(key string) (uint64, error) {
	if s.opts.Settings == nil {
		return 0, nil
	}
	cur, err := s.opts.Settings.Settings()
	if err != nil {
		return 0, err
	}
	var ids []uint64
	for k := range cur.Settings.Agents.Projects[key].PublishProfile.Targets {
		if v, ok := strings.CutPrefix(k, "steam:"); ok {
			if id, err := strconv.ParseUint(v, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) != 1 {
		return 0, nil
	}
	return ids[0], nil
}
