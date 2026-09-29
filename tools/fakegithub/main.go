// Command fakegithub serves the in-memory GitHub fake (internal/provider/github/githubtest)
// for live headless runs of IssueWatcher. Dev/E2E only; nothing reaches github.com.
//
//	go run ./tools/fakegithub -addr 127.0.0.1:18090 -repo octo/demo -seed <dataDir>
//	$env:IW_GITHUB_API = "http://127.0.0.1:18090"; $env:IW_DATA_DIR = "<dataDir>"; $env:IW_HEADLESS = "1"
//
// -seed writes a registered app + a non-expiring user token for the fake into
// <dataDir>\secrets, so the instance starts signed in. Control endpoints:
//
//	POST /_fake/issues  {"repo":"o/r","title":"...","labels":["bug"],"author":"x"} → the new issue
//	GET  /_fake/state   issues, repo labels, label add count, every call received
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
	"github.com/UberMorgott/issuewatcher/internal/secret"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18090", "listen address")
	repos := flag.String("repo", "octo/demo", "comma-separated owner/name repos of installation 1")
	labels := flag.String("labels", "bug,enhancement,question,documentation", "repo labels (every repo)")
	seed := flag.String("seed", "", "IssueWatcher data dir to sign in (writes secrets)")
	flag.Parse()

	s := githubtest.NewUnstarted()
	s.ExpiresIn = 0 // non-expiring user tokens: no refresh during a long run
	s.Labels = map[string][]string{}
	for r := range strings.SplitSeq(*repos, ",") {
		s.Repos = append(s.Repos, r)
		s.Labels[r] = strings.Split(*labels, ",")
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	s.Listener = ln
	fake := s.Config.Handler
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/_fake/issues":
			addIssue(s, w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/_fake/state":
			state(s, w)
		default:
			fake.ServeHTTP(w, r)
		}
	})
	if *seed != "" {
		if err := seedSecrets(s, *seed); err != nil {
			log.Fatal(err)
		}
	}
	s.Start()
	defer s.Close()
	fmt.Println("fakegithub listening on", s.URL) //nolint:forbidigo // CLI output
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}

func seedSecrets(s *githubtest.Server, dataDir string) error {
	dir := filepath.Join(dataDir, "secrets")
	app := github.App{ID: 42, Slug: "issuewatcher-test", Name: "IssueWatcher-test", ClientID: githubtest.ClientID,
		ClientSecret: githubtest.ClientSecret, PEM: "fake-pem", CreatedAt: time.Now()}
	if err := secret.WriteJSON(filepath.Join(dir, "github-app.json"), app); err != nil {
		return err
	}
	return secret.WriteJSON(filepath.Join(dir, "github-token.json"), github.Token{AccessToken: s.Grant(), Login: githubtest.Login})
}

func addIssue(s *githubtest.Server, w http.ResponseWriter, r *http.Request) {
	var in struct {
		Repo   string   `json:"repo"`
		Title  string   `json:"title"`
		Author string   `json:"author"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Title == "" {
		http.Error(w, "want {repo, title, labels, author}", http.StatusBadRequest)
		return
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if in.Repo == "" {
		in.Repo = s.Repos[0]
	}
	if in.Author == "" {
		in.Author = "reporter"
	}
	n := 1
	for _, is := range s.Issues {
		if is.Repo == in.Repo && is.Number >= n {
			n = is.Number + 1
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	is := &githubtest.Issue{ID: fmt.Sprintf("I_%s_%d", strings.ReplaceAll(in.Repo, "/", "_"), n), Repo: in.Repo, Number: n,
		Title: in.Title, Author: in.Author, Open: true, Labels: append([]string{}, in.Labels...), CreatedAt: now, UpdatedAt: now}
	s.Issues = append(s.Issues, is)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(issueJSON(is))
}

func state(s *githubtest.Server, w http.ResponseWriter) {
	calls := s.Calls()
	s.Mu.Lock()
	defer s.Mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"issues": issuesJSON(s.Issues), "labels": s.Labels, "labelAdds": s.LabelAdds, "calls": calls})
}

func issueJSON(is *githubtest.Issue) map[string]any {
	return map[string]any{"repo": is.Repo, "number": is.Number, "title": is.Title, "author": is.Author, "open": is.Open,
		"labels": is.Labels, "comments": len(is.Comments), "updatedAt": is.UpdatedAt}
}

func issuesJSON(list []*githubtest.Issue) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, is := range list {
		out = append(out, issueJSON(is))
	}
	return out
}
