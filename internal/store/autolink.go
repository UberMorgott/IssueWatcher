package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// Auto-linking (migration 010): a mod page whose name (or page slug) is the
// name of exactly one GitHub repository is linked to it without asking; a
// mod page with several such repositories, or only a partial name match, gets
// them as one-click suggestions (Repo.Suggest). A mod page is looked at until
// its link is decided (auto-linked, or linked / unlinked by hand) — the
// linker never overrides the user.

// normName keeps lowercase letters and digits: "Crossbow Save-Arrow" → "crossbowsavearrow".
func normName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// modNames are a mod page's comparable names: its title and the page slug
// (CurseForge /mods/<slug>; numeric ids of Nexus / Steam are no names).
func modNames(r Repo) []string {
	out := []string{normName(r.Name)}
	if u, err := url.Parse(r.URL); err == nil {
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		if slug := normName(segs[len(segs)-1]); slug != "" && strings.Trim(slug, "0123456789") != "" && slug != out[0] {
			out = append(out, slug)
		}
	}
	return out
}

// repoName is a code project's comparable name: the repository part of owner/repo.
func repoName(r Repo) string {
	n := r.Name
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return normName(n)
}

// minLoose is the shortest name a partial match may rest on.
const minLoose = 4

// matchCode returns the code projects whose name equals one of the mod page's
// names (exact) and those where one contains the other (loose, not exact).
func matchCode(mod Repo, codes []Repo) (exact, loose []int64) {
	names := modNames(mod)
	for _, c := range codes {
		rn := repoName(c)
		if rn == "" {
			continue
		}
		hit, part := false, false
		for _, n := range names {
			switch {
			case n == "":
			case n == rn:
				hit = true
			case min(len(n), len(rn)) >= minLoose && (strings.Contains(n, rn) || strings.Contains(rn, n)):
				part = true
			}
		}
		switch {
		case hit:
			exact = append(exact, c.ID)
		case part:
			loose = append(loose, c.ID)
		}
	}
	return exact, loose
}

// linkPlan splits the undecided, unlinked mod pages of repos into automatic
// links (mod → code) and suggestions (mod → code candidates).
func linkPlan(repos []Repo, decided map[int64]bool) (auto map[int64]int64, suggest map[int64][]int64) {
	auto, suggest = map[int64]int64{}, map[int64][]int64{}
	var codes []Repo
	for _, r := range repos {
		if r.Platform == CodePlatform {
			codes = append(codes, r)
		}
	}
	for _, r := range repos {
		if r.Platform == CodePlatform || r.LinkedTo != 0 || decided[r.ID] {
			continue
		}
		exact, loose := matchCode(r, codes)
		switch {
		case len(exact) == 1:
			auto[r.ID] = exact[0]
		case len(exact) > 1:
			suggest[r.ID] = exact
		case len(loose) > 0:
			suggest[r.ID] = loose
		}
	}
	return auto, suggest
}

func (s *Store) linkDecisions(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT mod_project_id FROM project_link_decisions`)
	if err != nil {
		return nil, fmt.Errorf("store: link decisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan decision: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// AutoLink links every undecided mod page with exactly one matching GitHub
// repository and records the decision. It returns the links made.
func (s *Store) AutoLink(ctx context.Context) (int, error) {
	repos, err := s.Repos(ctx)
	if err != nil {
		return 0, err
	}
	decided, err := s.linkDecisions(ctx)
	if err != nil {
		return 0, err
	}
	auto, _ := linkPlan(repos, decided)
	n := 0
	for mod, code := range auto {
		// A link made meanwhile by hand wins (DO NOTHING); the decision is recorded either way.
		res, err := s.db.ExecContext(ctx, `INSERT INTO project_links (mod_project_id, code_project_id) VALUES (?, ?)
			ON CONFLICT (mod_project_id) DO NOTHING`, mod, code)
		if err != nil {
			return n, fmt.Errorf("store: auto-link %d: %w", mod, err)
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
		if err := s.decideLink(ctx, s.db, mod); err != nil {
			return n, err
		}
	}
	return n, nil
}

// decideLink records that mod page id's link was decided (q: the db or a tx).
func (s *Store) decideLink(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, id int64,
) error {
	if _, err := q.ExecContext(ctx, `INSERT INTO project_link_decisions (mod_project_id) VALUES (?) ON CONFLICT DO NOTHING`, id); err != nil {
		return fmt.Errorf("store: link decision %d: %w", id, err)
	}
	return nil
}

// fillSuggestions sets Repo.Suggest of undecided, unlinked mod pages.
func (s *Store) fillSuggestions(ctx context.Context, repos []Repo) error {
	decided, err := s.linkDecisions(ctx)
	if err != nil {
		return err
	}
	_, suggest := linkPlan(repos, decided)
	for i := range repos {
		repos[i].Suggest = suggest[repos[i].ID]
	}
	return nil
}
