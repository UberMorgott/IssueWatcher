package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Pull is a pull request opened through the fake REST API.
type Pull struct {
	Repo   string
	Number int
	Title  string
	Head   string // branch name
	Base   string
	Body   string
	Draft  bool
}

func (s *Server) repo(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	branch := s.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	full := r.PathValue("owner") + "/" + r.PathValue("repo")
	writeJSON(w, http.StatusOK, map[string]any{"full_name": full, "default_branch": branch})
}

func (s *Server) pullJSON(p *Pull) map[string]any {
	return map[string]any{"number": p.Number, "html_url": fmt.Sprintf("%s/%s/pull/%d", s.URL, p.Repo, p.Number),
		"title": p.Title, "draft": p.Draft, "head": map[string]string{"ref": p.Head}, "base": map[string]string{"ref": p.Base}}
}

// listPulls answers ?head=owner:branch (any state).
func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	_, head, _ := strings.Cut(r.URL.Query().Get("head"), ":")
	out := []map[string]any{}
	for _, p := range s.Pulls {
		if p.Repo == repo && (head == "" || p.Head == head) {
			out = append(out, s.pullJSON(p))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createPull(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Head == "" || in.Base == "" || in.Title == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Validation Failed"})
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	for _, p := range s.Pulls {
		if p.Repo == repo && p.Head == in.Head {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "A pull request already exists for " + in.Head})
			return
		}
	}
	n := 100 + len(s.Pulls) + 1
	p := &Pull{Repo: repo, Number: n, Title: in.Title, Head: in.Head, Base: in.Base, Body: in.Body, Draft: in.Draft}
	s.Pulls = append(s.Pulls, p)
	writeJSON(w, http.StatusCreated, s.pullJSON(p))
}
