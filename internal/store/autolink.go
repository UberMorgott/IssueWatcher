package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Auto-linking (migrations 010, 011): a mod page is linked to a GitHub
// repository without asking when the match is certain; otherwise the
// candidates become one-click suggestions (Repo.Suggest). Certain, strongest
// first:
//  1. the mod page's own text links to the repository (Project.CodeURL);
//  2. a name of the mod page (title, title before " - " / ":", page slug) is
//     the name or core of exactly one repository — the core drops a
//     "<Game>-Mod(s)-" prefix and a "-Fork" suffix — or of exactly one
//     repository of the mod page's game — never one whose prefix names
//     another game than the page URL's (Skyrim page ≠ Fallout4-Mod-…);
//  3. exactly one repository of the mod page's game has the name as whole
//     words of its core (Oracle → PhoenixPoint-Mod-PerkOracle).
// The game comes from the page URL (CurseForge, Nexus: /<game>/mods/…) or,
// for Steam ("steam:<appid>"), from the repositories that other mod pages of
// the same app are linked to or certainly match. A mod page is looked at until
// its link is decided (auto-linked, or linked / unlinked by hand) — the linker
// never overrides the user.

// linkHint is what a provider saw on a mod page besides its name (projects.game, code_url).
type linkHint struct{ game, codeURL string }

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

// minLoose is the shortest name a partial match (or a title head) may rest on.
const minLoose = 4

// titleSeps end the short name at the head of a mod page title ("ShareShip - Open Any Ship's UI").
var titleSeps = []string{" - ", " – ", " — ", ": ", " | ", " ("}

// modNames are a mod page's comparable names: its title, the title's head and
// the page slug (/mods/<slug>; numeric ids of Nexus / Steam are no names).
func modNames(r Repo) []string {
	var out []string
	add := func(s string, minLen int) {
		if n := normName(s); len(n) >= minLen && strings.Trim(n, "0123456789") != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	add(r.Name, 1)
	head := r.Name
	for _, sep := range titleSeps {
		if i := strings.Index(head, sep); i > 0 {
			head = head[:i]
		}
	}
	add(head, minLoose)
	if segs := urlSegs(r.URL); len(segs) >= 2 && segs[len(segs)-2] == "mods" {
		add(segs[len(segs)-1], 1)
	}
	return out
}

func urlSegs(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return strings.Split(strings.Trim(u.Path, "/"), "/")
}

// modGame is a mod page's game key: the provider's (Steam "steam:<appid>") or
// the normalized /<game>/mods/ segment of its URL; "" = unknown.
func modGame(r Repo, h linkHint) string {
	if h.game != "" {
		return h.game
	}
	if segs := urlSegs(r.URL); len(segs) >= 3 && segs[1] == "mods" {
		return normName(segs[0])
	}
	return ""
}

// codeRepo is a GitHub repository's comparable parts.
type codeRepo struct {
	id               int64
	path             string   // lowercase owner/repo
	full, core, game string   // normalized repo name, name without prefix/suffix, prefix game
	words            []string // lowercase words of the core ("PerkOracle" → perk, oracle)
}

var modPrefix = regexp.MustCompile(`(?i)^(.+?)-mods?-(.+)$`)

func parseCode(r Repo) codeRepo {
	part := r.Name
	if i := strings.LastIndex(part, "/"); i >= 0 {
		part = part[i+1:]
	}
	c := codeRepo{id: r.ID, path: strings.ToLower(r.Name), full: normName(part)}
	rest := part
	if m := modPrefix.FindStringSubmatch(part); m != nil {
		c.game, rest = normName(m[1]), m[2]
	}
	if n := len(rest) - len("-fork"); n > 0 && strings.EqualFold(rest[n:], "-fork") {
		rest = rest[:n]
	}
	c.core, c.words = normName(rest), splitWords(rest)
	return c
}

// splitWords splits at separators and CamelCase / letter-digit boundaries:
// "PPCli_v2" → pp, cli, v, 2.
func splitWords(s string) []string {
	var (
		out []string
		cur []rune
	)
	rs := []rune(s)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(cur) > 0 {
			p := cur[len(cur)-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if (unicode.IsLower(p) && unicode.IsUpper(r)) ||
				(unicode.IsUpper(p) && unicode.IsUpper(r) && nextLower) ||
				unicode.IsDigit(p) != unicode.IsDigit(r) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// hasWords reports whether name is a run of whole words of words.
func hasWords(words []string, name string) bool {
	for i := range words {
		rest := name
		for _, w := range words[i:] {
			var ok bool
			if rest, ok = strings.CutPrefix(rest, w); !ok {
				break
			}
			if rest == "" {
				return true
			}
		}
	}
	return false
}

// matchCode returns the repositories whose name or core equals one of the mod
// page's names (exact), whose core has one as whole words (words), and those
// where one name contains the other (loose, not exact).
func matchCode(names []string, codes []codeRepo) (exact, words, loose []int64) {
	for _, c := range codes {
		if c.full == "" {
			continue
		}
		hit, word, part := false, false, false
		for _, n := range names {
			switch {
			case n == c.full || n == c.core:
				hit = true
			case len(n) >= minLoose && hasWords(c.words, n):
				word = true
				fallthrough
			case len(n) >= minLoose && (strings.Contains(c.full, n) || min(len(n), len(c.core)) >= minLoose && strings.Contains(n, c.core)):
				part = true
			}
		}
		switch {
		case hit:
			exact = append(exact, c.id)
		case part:
			loose = append(loose, c.id)
		}
		if word && !hit {
			words = append(words, c.id)
		}
	}
	return exact, words, loose
}

// sameGame reports whether mod game key and repository prefix game name the
// same game; one may extend the other ("skyrimspecialedition" / "skyrim").
func sameGame(key, game string) bool {
	return key == game || strings.HasPrefix(key, game) || strings.HasPrefix(game, key)
}

// otherGame reports whether a repository of prefix game is certainly for
// another game than mod game key: both known and different. A Steam key
// ("steam:<appid>") is no game name and only learns its game, so it never is.
func otherGame(key, game string) bool {
	return key != "" && game != "" && !strings.Contains(key, ":") && !sameGame(key, game)
}

// linkPlan splits the undecided, unlinked mod pages of repos into automatic
// links (mod → code) and suggestions (mod → code candidates).
func linkPlan(repos []Repo, decided map[int64]bool, hints map[int64]linkHint) (auto map[int64]int64, suggest map[int64][]int64) {
	auto, suggest = map[int64]int64{}, map[int64][]int64{}
	var codes []codeRepo
	byID, byPath := map[int64]codeRepo{}, map[string]int64{}
	for _, r := range repos {
		if r.Platform == CodePlatform {
			c := parseCode(r)
			codes = append(codes, c)
			byID[c.id], byPath[c.path] = c, c.id
		}
	}
	// learned: mod game key → games of the repositories its mod pages belong to.
	learned := map[string]map[string]bool{}
	learn := func(key string, code int64) {
		if g := byID[code].game; key != "" && g != "" {
			if learned[key] == nil {
				learned[key] = map[string]bool{}
			}
			learned[key][g] = true
		}
	}
	type open struct {
		mod                 int64
		key                 string
		exact, words, loose []int64
	}
	var rest []open
	for _, r := range repos {
		if r.Platform == CodePlatform {
			continue
		}
		key := modGame(r, hints[r.ID])
		if r.LinkedTo != 0 {
			learn(key, r.LinkedTo)
			continue
		}
		if decided[r.ID] {
			continue
		}
		if h := hints[r.ID].codeURL; h != "" {
			if id, ok := byPath[strings.ToLower(strings.TrimPrefix(h, "https://github.com/"))]; ok {
				auto[r.ID] = id
				learn(key, id)
				continue
			}
		}
		exact, words, loose := matchCode(modNames(r), codes)
		if len(exact) == 1 && !otherGame(key, byID[exact[0]].game) {
			auto[r.ID] = exact[0]
			learn(key, exact[0])
			continue
		}
		rest = append(rest, open{r.ID, key, exact, words, loose})
	}
	for _, o := range rest {
		inGame := func(ids []int64) (out []int64) {
			for _, id := range ids {
				g := byID[id].game
				if g == "" || o.key == "" {
					continue
				}
				if sameGame(o.key, g) || (len(learned[o.key]) == 1 && learned[o.key][g]) {
					out = append(out, id)
				}
			}
			return out
		}
		if len(o.exact) > 0 { // one exact match reaches here only when of another game
			if g := inGame(o.exact); len(g) == 1 {
				auto[o.mod] = g[0]
			} else {
				suggest[o.mod] = o.exact
			}
			continue
		}
		if g := inGame(o.words); len(g) == 1 && len(inGame(o.loose)) == 1 {
			auto[o.mod] = g[0]
			continue
		}
		if len(o.loose) > 0 {
			suggest[o.mod] = o.loose
		}
	}
	return auto, suggest
}

// linkHints are the mod pages' provider hints by project id.
func (s *Store) linkHints(ctx context.Context) (map[int64]linkHint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, game, code_url FROM projects WHERE active = 1 AND (game <> '' OR code_url <> '')`)
	if err != nil {
		return nil, fmt.Errorf("store: link hints: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]linkHint{}
	for rows.Next() {
		var (
			id int64
			h  linkHint
		)
		if err := rows.Scan(&id, &h.game, &h.codeURL); err != nil {
			return nil, fmt.Errorf("store: scan hint: %w", err)
		}
		out[id] = h
	}
	return out, rows.Err()
}

// plan runs linkPlan over the stored decisions and hints.
func (s *Store) plan(ctx context.Context, repos []Repo) (auto map[int64]int64, suggest map[int64][]int64, err error) {
	decided, err := s.linkDecisions(ctx)
	if err != nil {
		return nil, nil, err
	}
	hints, err := s.linkHints(ctx)
	if err != nil {
		return nil, nil, err
	}
	auto, suggest = linkPlan(repos, decided, hints)
	return auto, suggest, nil
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

// AutoLink links every undecided mod page with a certain GitHub repository
// match and records the decision. It returns the links made.
func (s *Store) AutoLink(ctx context.Context) (int, error) {
	repos, err := s.Repos(ctx)
	if err != nil {
		return 0, err
	}
	auto, _, err := s.plan(ctx, repos)
	if err != nil {
		return 0, err
	}
	return s.applyAutoLinks(ctx, auto)
}

// applyAutoLinks writes planned links (mod → code) and records each decision.
// A decision made meanwhile by hand (link, or unlink after the plan was read)
// wins: the link is inserted only while the mod page is still undecided, in
// the same transaction that records the decision.
func (s *Store) applyAutoLinks(ctx context.Context, auto map[int64]int64) (int, error) {
	n := 0
	for mod, code := range auto {
		k, err := s.autoLinkOne(ctx, mod, code)
		if err != nil {
			return n, err
		}
		n += k
	}
	return n, nil
}

func (s *Store) autoLinkOne(ctx context.Context, mod, code int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO project_links (mod_project_id, code_project_id)
		SELECT ?1, ?2 WHERE NOT EXISTS (SELECT 1 FROM project_link_decisions WHERE mod_project_id = ?1)
		ON CONFLICT (mod_project_id) DO NOTHING`, mod, code)
	if err != nil {
		return 0, fmt.Errorf("store: auto-link %d: %w", mod, err)
	}
	k, _ := res.RowsAffected()
	if err := s.decideLink(ctx, tx, mod); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit auto-link %d: %w", mod, err)
	}
	return int(k), nil
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
	_, suggest, err := s.plan(ctx, repos)
	if err != nil {
		return err
	}
	for i := range repos {
		repos[i].Suggest = suggest[repos[i].ID]
	}
	return nil
}
