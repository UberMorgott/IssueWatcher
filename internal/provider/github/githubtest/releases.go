package githubtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Release is a release created through (or seeded into) the fake REST API.
type Release struct {
	ID              int64
	Repo            string
	TagName         string
	TargetCommitish string
	Name            string
	Body            string
	Draft           bool
	Prerelease      bool
	Assets          []*Asset
}

// Asset is an uploaded release asset; Data is the exact request body.
type Asset struct {
	ID          int64
	Name        string
	ContentType string
	Data        []byte
}

func (s *Server) assetJSON(rel *Release, a *Asset) map[string]any {
	sum := sha256.Sum256(a.Data)
	return map[string]any{"id": a.ID, "name": a.Name, "size": len(a.Data), "state": "uploaded",
		"content_type": a.ContentType, "digest": "sha256:" + hex.EncodeToString(sum[:]),
		"browser_download_url": fmt.Sprintf("%s/%s/releases/download/%s/%s", s.URL, rel.Repo, rel.TagName, a.Name)}
}

func (s *Server) releaseJSON(rel *Release) map[string]any {
	assets := []map[string]any{}
	for _, a := range rel.Assets {
		assets = append(assets, s.assetJSON(rel, a))
	}
	return map[string]any{"id": rel.ID, "tag_name": rel.TagName, "name": rel.Name, "body": rel.Body,
		"draft": rel.Draft, "prerelease": rel.Prerelease, "target_commitish": rel.TargetCommitish,
		"html_url":   fmt.Sprintf("%s/%s/releases/tag/%s", s.URL, rel.Repo, rel.TagName),
		"upload_url": fmt.Sprintf("%s/repos/%s/releases/%d/assets{?name,label}", s.URL, rel.Repo, rel.ID),
		"assets":     assets}
}

// releaseByTag answers GET /repos/{o}/{r}/releases/tags/{tag} (published only).
func (s *Server) releaseByTag(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	for _, rel := range s.Releases {
		if rel.Repo == repo && rel.TagName == r.PathValue("tag") && !rel.Draft {
			writeJSON(w, http.StatusOK, s.releaseJSON(rel))
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (s *Server) createRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TagName         string `json:"tag_name"`
		TargetCommitish string `json:"target_commitish"`
		Name            string `json:"name"`
		Body            string `json:"body"`
		Draft           bool   `json:"draft"`
		Prerelease      bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.TagName == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	for _, rel := range s.Releases {
		if rel.Repo == repo && rel.TagName == in.TagName {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed: tag_name already_exists"})
			return
		}
	}
	rel := &Release{ID: int64(1000 + len(s.Releases) + 1), Repo: repo, TagName: in.TagName, TargetCommitish: in.TargetCommitish,
		Name: in.Name, Body: in.Body, Draft: in.Draft, Prerelease: in.Prerelease}
	s.Releases = append(s.Releases, rel)
	writeJSON(w, http.StatusCreated, s.releaseJSON(rel))
}

// uploadAsset answers POST /repos/{o}/{r}/releases/{id}/assets?name= (the
// uploads host on GitHub; the fake's upload_url points here).
func (s *Server) uploadAsset(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	name := r.URL.Query().Get("name")
	if err != nil || name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Bad Request"})
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.Mu.Lock()
	defer s.Mu.Unlock()
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	for _, rel := range s.Releases {
		if rel.Repo != repo || rel.ID != id {
			continue
		}
		for _, a := range rel.Assets {
			if a.Name == name {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed: already_exists"})
				return
			}
		}
		a := &Asset{ID: int64(5000 + len(rel.Assets) + 1), Name: name, ContentType: r.Header.Get("Content-Type"), Data: data}
		rel.Assets = append(rel.Assets, a)
		writeJSON(w, http.StatusCreated, s.assetJSON(rel, a))
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}
