package githubtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// REST change-check endpoints with ETag / If-None-Match, as GitHub serves them:
// the ETag is a hash of the body, so an unchanged answer for the same URL is a
// 304 without a body.

// Hits counts requests by kind for assertions.
type Hits struct {
	REST        int // change-check GETs (issues + comments)
	NotModified int // of them answered 304
	GraphQL     int
}

// Hits returns the request counters.
func (s *Server) Hits() Hits {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.hits
}

// FailNext makes the next n authenticated requests fail with status and headers
// (e.g. 403 + Retry-After for GitHub's secondary rate limit).
func (s *Server) FailNext(n, status int, headers map[string]string, message string) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.fail = &failure{n: n, status: status, headers: headers, message: message}
}

type failure struct {
	n       int
	status  int
	headers map[string]string
	message string
}

// injected answers with the pending failure, if any (Mu held).
func (s *Server) injected(w http.ResponseWriter) bool {
	if s.fail == nil || s.fail.n == 0 {
		return false
	}
	s.fail.n--
	for k, v := range s.fail.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.fail.status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": s.fail.message})
	return true
}

func since(r *http.Request) time.Time {
	t, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	return t
}

// writeConditional sends v with an ETag, or 304 when If-None-Match matches.
func (s *Server) writeConditional(w http.ResponseWriter, r *http.Request, v any) {
	body, _ := json.Marshal(v)
	sum := sha256.Sum256(body)
	etag := `W/"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("X-RateLimit-Remaining", "4999")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	if s.PollInterval > 0 {
		w.Header().Set("X-Poll-Interval", strconv.Itoa(s.PollInterval))
	}
	if r.Header.Get("If-None-Match") == etag {
		s.hits.NotModified++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (s *Server) restIssues(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.hits.REST++
	if s.injected(w) {
		return
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	from := since(r)
	var list []*Issue
	for _, is := range s.Issues {
		if is.Repo == repo && !is.UpdatedAt.Before(from) {
			list = append(list, is)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt.Before(list[j].UpdatedAt) })
	out := []map[string]any{}
	for _, is := range list[:min(len(list), perPage(r))] {
		out = append(out, map[string]any{"number": is.Number, "updated_at": is.UpdatedAt, "title": is.Title})
	}
	for _, pr := range s.PullRequests { // the issues endpoint lists PRs too
		if pr.Repo == repo && !pr.UpdatedAt.Before(from) {
			out = append(out, map[string]any{"number": pr.Number, "updated_at": pr.UpdatedAt, "pull_request": map[string]string{"url": "x"}})
		}
	}
	s.writeConditional(w, r, out)
}

func (s *Server) restComments(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.hits.REST++
	if s.injected(w) {
		return
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("repo")
	from := since(r)
	type row struct {
		n   int
		c   Comment
		url string
	}
	var rows []row
	for _, is := range s.Issues {
		if is.Repo != repo {
			continue
		}
		for _, c := range is.Comments {
			if !c.CreatedAt.Before(from) {
				rows = append(rows, row{is.Number, c, fmt.Sprintf("%s/repos/%s/issues/%d", s.URL, repo, is.Number)})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].c.CreatedAt.Before(rows[j].c.CreatedAt) })
	out := []map[string]any{}
	for _, x := range rows[:min(len(rows), perPage(r))] {
		out = append(out, map[string]any{"id": x.c.ID, "issue_url": x.url, "updated_at": x.c.CreatedAt, "body": x.c.Body})
	}
	s.writeConditional(w, r, out)
}

func perPage(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || n <= 0 || n > 100 {
		return 30
	}
	return n
}

// issuesByNumber answers `repository { iN: issue(number: X) {...} }` (aliased
// targeted fetches); unknown numbers (pull requests) are null.
func (s *Server) issuesByNumber(w http.ResponseWriter, query string, vars map[string]any) {
	repo := str(vars["owner"]) + "/" + str(vars["name"])
	out := map[string]any{}
	for line := range strings.SplitSeq(query, "\n") {
		alias, rest, ok := strings.Cut(strings.TrimSpace(line), ": issue(number: ")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(rest[:strings.Index(rest, ")")])
		out[alias] = nil
		for _, is := range s.Issues {
			if is.Repo == repo && is.Number == n {
				out[alias] = issueNode(is)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": out}})
}
