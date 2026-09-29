package github

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
)

func TestListAndAddLabels(t *testing.T) {
	gh := githubtest.New(t)
	seed(gh)
	var many []string
	for i := range 130 { // two REST pages of 100
		many = append(many, fmt.Sprintf("area-%03d", i))
	}
	gh.Mu.Lock()
	gh.Labels = map[string][]string{"octo/app": append([]string{"bug", "docs"}, many...)}
	gh.Mu.Unlock()
	a := newAuth(t, gh)
	signIn(t, gh, a)
	p := NewProvider(a)
	if !p.Capabilities().SetLabels {
		t.Fatal("SetLabels capability off")
	}

	labels, err := p.ListLabels(t.Context(), "octo/app")
	if err != nil || len(labels) != 132 || labels[1].Name != "docs" || labels[131].Name != "area-129" || labels[0].Color == "" {
		t.Fatalf("labels %d %v", len(labels), err)
	}
	if empty, err := p.ListLabels(t.Context(), "octo/lib"); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("no labels: %v %v", empty, err)
	}
	if _, err := p.ListLabels(t.Context(), "bad"); err == nil {
		t.Fatal("bad repo id accepted")
	}

	// Adding keeps the issue's labels; adding an existing one changes nothing.
	got, err := p.AddLabels(t.Context(), "octo/app", 1, []string{"docs"})
	if err != nil || !slices.Equal(got, []string{"bug", "docs"}) {
		t.Fatalf("add: %v %v", got, err)
	}
	if got, err = p.AddLabels(t.Context(), "octo/app", 1, []string{"bug", "docs"}); err != nil || !slices.Equal(got, []string{"bug", "docs"}) {
		t.Fatalf("add again: %v %v", got, err)
	}
	if _, err := p.AddLabels(t.Context(), "octo/app", 99, []string{"docs"}); err == nil {
		t.Fatal("missing issue accepted")
	}

	if err := a.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListLabels(t.Context(), "octo/app"); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("signed out: %v", err)
	}
	if _, err := p.AddLabels(t.Context(), "octo/app", 1, []string{"docs"}); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("add signed out: %v", err)
	}
}
