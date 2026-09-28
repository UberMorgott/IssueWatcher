// Package githubtest is an in-memory fake of the GitHub endpoints IssueWatcher
// uses (manifest conversion, OAuth token/device endpoints, REST installations,
// GraphQL issues/comments/addComment). Tests point Auth.WebURL and Auth.APIURL
// at Server.URL; nothing touches the network.
package githubtest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fake credentials: deliberately not shaped like real GitHub tokens.
const (
	ManifestCode = "manifest-code-1"
	ClientID     = "Iv-test-client"
	ClientSecret = "test-client-secret"
	Login        = "octo"
)

// Comment is a fake issue comment.
type Comment struct {
	ID        string
	Author    string
	Body      string
	CreatedAt time.Time
}

// Issue is a fake issue.
type Issue struct {
	ID        string
	Repo      string // owner/name
	Number    int
	Title     string
	Author    string
	Open      bool
	Labels    []string
	CreatedAt time.Time
	UpdatedAt time.Time
	ClosedAt  time.Time
	Comments  []Comment
}

// Server is the fake. Lock Mu before mutating Issues/Repos from a test.
type Server struct {
	*httptest.Server
	Mu sync.Mutex

	Repos     []string // owner/name reachable through installation 1
	Issues    []*Issue
	ExpiresIn int // seconds; 0 = non-expiring user tokens

	codes      map[string]string // auth code → PKCE challenge
	access     map[string]bool   // live access tokens
	refresh    map[string]bool   // live refresh tokens
	seq        int
	refreshes  int // successful refresh grants
	deviceHits int // device polls answered
}

// New starts the fake; it is closed with the test.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{codes: map[string]string{}, access: map[string]bool{}, refresh: map[string]bool{}, ExpiresIn: 28800}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app-manifests/{code}/conversions", s.conversion)
	mux.HandleFunc("POST /login/oauth/access_token", s.token)
	mux.HandleFunc("POST /login/device/code", s.deviceCode)
	mux.HandleFunc("GET /user", s.authed(s.user))
	mux.HandleFunc("GET /user/installations", s.authed(s.installations))
	mux.HandleFunc("GET /user/installations/{id}/repositories", s.authed(s.repositories))
	mux.HandleFunc("POST /graphql", s.authed(s.graphql))
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// IssueCode registers an authorization code bound to a PKCE verifier's challenge,
// as github.com/login/oauth/authorize would after the user clicks Authorize.
func (s *Server) IssueCode(challenge string) string {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.seq++
	code := "authcode-" + strconv.Itoa(s.seq)
	s.codes[code] = challenge
	return code
}

// Counts returns successful refresh grants and device polls answered.
func (s *Server) Counts() (refreshes, deviceHits int) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	return s.refreshes, s.deviceHits
}

// RevokeAccess invalidates every access token (forces 401).
func (s *Server) RevokeAccess() {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.access = map[string]bool{}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Remaining", "4999")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) conversion(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("code") != ManifestCode {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": 42, "slug": "issuewatcher-test", "node_id": "A_1", "name": "IssueWatcher-test",
		"html_url": s.URL + "/apps/issuewatcher-test", "owner": map[string]string{"login": Login},
		"client_id": ClientID, "client_secret": ClientSecret, "webhook_secret": nil, "pem": "fake-pem",
	})
}

func (s *Server) newTokens() map[string]any {
	s.seq++
	a, rt := "access-"+strconv.Itoa(s.seq), "refresh-"+strconv.Itoa(s.seq)
	s.access[a] = true
	out := map[string]any{"access_token": a, "token_type": "bearer", "scope": ""}
	if s.ExpiresIn > 0 {
		s.refresh[rt] = true
		out["expires_in"] = s.ExpiresIn
		out["refresh_token"] = rt
		out["refresh_token_expires_in"] = 15897600
	}
	return out
}

func oauthErr(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusOK, map[string]string{"error": code, "error_description": code})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthErr(w, "bad_request")
		return
	}
	f := r.PostForm
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if f.Get("client_id") != ClientID {
		oauthErr(w, "incorrect_client_credentials")
		return
	}
	switch f.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		s.deviceHits++
		if s.deviceHits < 2 {
			oauthErr(w, "authorization_pending")
			return
		}
		writeJSON(w, http.StatusOK, s.newTokens())
	case "refresh_token":
		if f.Get("client_secret") != ClientSecret {
			oauthErr(w, "incorrect_client_credentials")
			return
		}
		rt := f.Get("refresh_token")
		if !s.refresh[rt] {
			oauthErr(w, "bad_refresh_token")
			return
		}
		delete(s.refresh, rt) // single use
		s.refreshes++
		writeJSON(w, http.StatusOK, s.newTokens())
	default:
		challenge, ok := s.codes[f.Get("code")]
		if !ok || f.Get("client_secret") != ClientSecret {
			oauthErr(w, "bad_verification_code")
			return
		}
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			oauthErr(w, "invalid_grant")
			return
		}
		delete(s.codes, f.Get("code"))
		writeJSON(w, http.StatusOK, s.newTokens())
	}
}

func (s *Server) deviceCode(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": "dev-1", "user_code": "ABCD-1234", "verification_uri": s.URL + "/login/device",
		"expires_in": 900, "interval": 0,
	})
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.Mu.Lock()
		ok := s.access[tok]
		s.Mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		h(w, r)
	}
}

func (s *Server) user(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"login": Login, "avatar_url": "https://avatars.example/octo"})
}

func (s *Server) installations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"total_count": 1, "installations": []map[string]any{{"id": 1}}})
}

func (s *Server) repositories(w http.ResponseWriter, r *http.Request) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	repos := []map[string]string{}
	if r.PathValue("id") == "1" {
		for _, full := range s.Repos {
			repos = append(repos, map[string]string{"full_name": full, "html_url": s.URL + "/" + full})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(repos), "repositories": repos})
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func (s *Server) graphql(w http.ResponseWriter, r *http.Request) {
	var req gqlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad json"})
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	switch {
	case strings.Contains(req.Query, "addComment"):
		s.addComment(w, req.Variables)
	case strings.Contains(req.Query, "repository("):
		s.issues(w, req.Variables)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"errors": []map[string]string{{"message": "unsupported query"}}})
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func (s *Server) issues(w http.ResponseWriter, vars map[string]any) {
	repo := str(vars["owner"]) + "/" + str(vars["name"])
	var since time.Time
	if v := str(vars["since"]); v != "" {
		since, _ = time.Parse(time.RFC3339, v)
	}
	var list []*Issue
	for _, is := range s.Issues {
		if is.Repo == repo && !is.UpdatedAt.Before(since) {
			list = append(list, is)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt.Before(list[j].UpdatedAt) })
	start, _ := strconv.Atoi(str(vars["after"]))
	const pageSize = 2 // small pages exercise pagination
	end := min(start+pageSize, len(list))
	nodes := []map[string]any{}
	for _, is := range list[start:end] {
		nodes = append(nodes, issueNode(is))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{
		"issues": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end < len(list), "endCursor": strconv.Itoa(end)},
			"nodes":    nodes,
		},
	}}})
}

func issueNode(is *Issue) map[string]any {
	state, closedAt := "OPEN", any(nil)
	if !is.Open {
		state, closedAt = "CLOSED", is.ClosedAt
	}
	labels := []map[string]string{}
	for _, l := range is.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	comments := []map[string]any{}
	for _, c := range is.Comments {
		comments = append(comments, map[string]any{
			"id": c.ID, "body": c.Body, "url": "u", "createdAt": c.CreatedAt, "updatedAt": c.CreatedAt,
			"author": map[string]string{"login": c.Author},
		})
	}
	return map[string]any{
		"id": is.ID, "number": is.Number, "title": is.Title, "body": "", "url": "https://x/" + is.ID,
		"state": state, "stateReason": nil, "createdAt": is.CreatedAt, "updatedAt": is.UpdatedAt, "closedAt": closedAt,
		"author": map[string]string{"login": is.Author}, "labels": map[string]any{"nodes": labels},
		"comments": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": comments},
	}
}

func (s *Server) addComment(w http.ResponseWriter, vars map[string]any) {
	id := str(vars["id"])
	for _, is := range s.Issues {
		if is.ID != id {
			continue
		}
		s.seq++
		now := time.Now().UTC().Truncate(time.Second)
		c := Comment{ID: fmt.Sprintf("IC_%d", s.seq), Author: Login, Body: str(vars["body"]), CreatedAt: now}
		is.Comments = append(is.Comments, c)
		is.UpdatedAt = now
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"addComment": map[string]any{
			"commentEdge": map[string]any{"node": map[string]any{
				"id": c.ID, "body": c.Body, "url": "u", "createdAt": now, "updatedAt": now,
				"author": map[string]string{"login": Login},
			}},
		}}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"errors": []map[string]string{{"message": "not found"}}})
}
