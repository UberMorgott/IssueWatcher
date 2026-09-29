package githubtest

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

func labelJSON(name string) map[string]string {
	return map[string]string{"name": name, "color": "ededed", "description": ""}
}

// listLabels pages the repository's labels (per_page, page).
func (s *Server) listLabels(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	all := s.Labels[r.PathValue("owner")+"/"+r.PathValue("repo")]
	per := perPage(r)
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	start := min((page-1)*per, len(all))
	out := []map[string]string{}
	for _, l := range all[start:min(start+per, len(all))] {
		out = append(out, labelJSON(l))
	}
	writeJSON(w, http.StatusOK, out)
}

// addLabels adds to an issue's labels, as GitHub does: existing ones stay, a
// name the repository lacks is created (matching is case-insensitive).
func (s *Server) addLabels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Problems parsing JSON"})
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.LabelAdds++
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	n, _ := strconv.Atoi(r.PathValue("number"))
	for _, is := range s.Issues {
		if is.Repo != repo || is.Number != n {
			continue
		}
		for _, name := range in.Labels {
			if !slices.ContainsFunc(s.Labels[repo], func(l string) bool { return strings.EqualFold(l, name) }) {
				if s.Labels == nil {
					s.Labels = map[string][]string{}
				}
				s.Labels[repo] = append(s.Labels[repo], name)
			}
			if !slices.ContainsFunc(is.Labels, func(l string) bool { return strings.EqualFold(l, name) }) {
				is.Labels = append(is.Labels, name)
				is.UpdatedAt = time.Now().UTC().Truncate(time.Second) // GitHub bumps updated_at
			}
		}
		out := []map[string]string{}
		for _, l := range is.Labels {
			out = append(out, labelJSON(l))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}
