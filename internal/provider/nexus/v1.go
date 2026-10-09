package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// The public v1 API (https://api.nexusmods.com/v1) answers only with the
// apikey header: every v1 read goes through getV1, which sends the stored key
// (Settings › Платформы › Nexus) and keeps it out of every error.

// getV1 GETs path (e.g. /games/wartales/mods/202/changelogs.json) and decodes
// the JSON answer into out. A 429/503 is retried once.
func (k *Keys) getV1(ctx context.Context, path string, out any) error {
	key, err := k.Key()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(k.opts.V1, "/")+path, nil)
		if err != nil {
			return err
		}
		setAPIHeaders(req.Header, key, k.opts.Version)
		res, err := k.opts.HTTP.Do(req)
		if err != nil {
			return fmt.Errorf("nexus v1: GET %s: %w", path, redactErr(err, key))
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		_ = res.Body.Close()
		if attempt == 0 && (res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable) {
			if err := sleepCtx(ctx, retryAfter(res.Header.Get("Retry-After"))); err != nil {
				return err
			}
			continue
		}
		switch {
		case res.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("nexus v1: GET %s: %w", path, badKeyErr(res.StatusCode))
		case res.StatusCode != http.StatusOK:
			return fmt.Errorf("nexus v1: GET %s: HTTP %d: %s", path, res.StatusCode, redact(snippet(body), key))
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("nexus v1: GET %s: unexpected answer: %s", path, redact(snippet(body), key))
		}
		return nil
	}
}

// v1ChangelogPath is the v1 changelogs.json of a mod.
func v1ChangelogPath(game string, mod int) string {
	return "/games/" + url.PathEscape(game) + "/mods/" + strconv.Itoa(mod) + "/changelogs.json"
}

// v1Changelogs is GET changelogs.json: version → its lines ([] = none).
func (k *Keys) v1Changelogs(ctx context.Context, game string, mod int) (map[string][]string, error) {
	var raw json.RawMessage
	if err := k.getV1(ctx, v1ChangelogPath(game, mod), &raw); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" || strings.HasPrefix(t, "[") {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("nexus v1: changelogs.json: %w", err)
	}
	return out, nil
}

// changelogNorm makes an editor entry and a v1 line comparable: line breaks
// (<br /> or \n) and runs of spaces become one space, entities decoded.
func changelogNorm(s string) string {
	s = changelogBR.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// cacheNote is the note on a mismatch: the public API may serve a cached copy.
const cacheNote = "публичный API Nexus (v1) может отдавать кэшированную копию несколько минут: " +
	"расхождение сразу после изменения не ошибка, проверьте позже (changelog check)"

// compareChangelogs compares the editor's versions (want) with v1 (got);
// edited names the version a set/delete changed ("" = none).
func compareChangelogs(source string, want []provider.ChangelogVersion, got map[string][]string, edited string) provider.ChangelogCheck {
	out := provider.ChangelogCheck{Source: source, Match: true, Versions: []provider.ChangelogVersionCheck{}}
	seen := map[string]bool{}
	add := func(version string, exp []string) {
		seen[version] = true
		pub := got[version]
		vc := provider.ChangelogVersionCheck{Version: version, Edited: version == edited, Expected: len(exp), Public: len(pub)}
		ne, np := make([]string, len(exp)), make([]string, len(pub))
		for i, s := range exp {
			ne[i] = changelogNorm(s)
		}
		for i, s := range pub {
			np[i] = changelogNorm(s)
		}
		vc.Missing, vc.Extra = multisetDiff(ne, np), multisetDiff(np, ne)
		vc.Match = len(ne) == len(np) && len(vc.Missing) == 0 && len(vc.Extra) == 0
		if !vc.Match {
			out.Match = false
		}
		out.Versions = append(out.Versions, vc)
	}
	for _, v := range want {
		exp := make([]string, len(v.Entries))
		for i, e := range v.Entries {
			exp[i] = e.Text
		}
		add(v.Version, exp)
	}
	var rest []provider.ChangelogVersion
	for v := range got {
		if !seen[v] {
			rest = append(rest, provider.ChangelogVersion{Version: v})
		}
	}
	sortVersionsDesc(rest)
	for _, v := range rest {
		add(v.Version, nil)
	}
	if edited != "" && !seen[edited] {
		add(edited, nil) // deleted, and v1 has it gone too
	}
	if !out.Match {
		out.Note = cacheNote
	}
	return out
}

// multisetDiff is a minus b, counting repeats.
func multisetDiff(a, b []string) []string {
	n := map[string]int{}
	for _, s := range b {
		n[s]++
	}
	var out []string
	for _, s := range a {
		if n[s] > 0 {
			n[s]--
			continue
		}
		out = append(out, s)
	}
	return out
}

// checkFailed is a check whose v1 read failed: the code, the redacted error.
func checkFailed(source string, err error) provider.ChangelogCheck {
	code := "check_failed"
	switch {
	case errors.Is(err, ErrNoAPIKey):
		code = "no_api_key"
	case errors.Is(err, ErrBadAPIKey):
		code = "bad_api_key"
	}
	return provider.ChangelogCheck{Source: source, Versions: []provider.ChangelogVersionCheck{}, Code: code, Error: err.Error()}
}

// v1Check reads v1 changelogs.json and compares it with want.
func (p *Provider) v1Check(ctx context.Context, t changelogTarget, want []provider.ChangelogVersion, edited string) (provider.ChangelogCheck, error) {
	source := strings.TrimRight(p.opts.Keys.opts.V1, "/") + v1ChangelogPath(t.game, t.mod)
	got, err := p.opts.Keys.v1Changelogs(ctx, t.game, t.mod)
	if err != nil {
		return checkFailed(source, err), err
	}
	return compareChangelogs(source, want, got, edited), nil
}

var _ provider.ChangelogChecker = (*Provider)(nil)

// CheckChangelogs implements provider.ChangelogChecker: the editor's
// changelog vs v1 changelogs.json (read with the stored API key).
func (p *Provider) CheckChangelogs(ctx context.Context, project provider.Project) (provider.ChangelogCheck, error) {
	if p.opts.Keys == nil {
		return provider.ChangelogCheck{}, errNoKey
	}
	n, err := p.site()
	if err != nil {
		return provider.ChangelogCheck{}, err
	}
	t, err := n.changelogTarget(ctx, project.ExternalID)
	if err != nil {
		return provider.ChangelogCheck{}, err
	}
	current, err := n.changelogs(ctx, t)
	if err != nil {
		return provider.ChangelogCheck{}, err
	}
	c, err := p.v1Check(ctx, t, current, "")
	if err != nil {
		return provider.ChangelogCheck{}, err
	}
	return c, nil
}

// afterChange is the editor's changelog once version reads want (nil = deleted).
func afterChange(current []provider.ChangelogVersion, version string, want []string) []provider.ChangelogVersion {
	out := []provider.ChangelogVersion{}
	found := false
	for _, v := range current {
		if v.Version != version {
			out = append(out, v)
			continue
		}
		found = true
		if want != nil {
			out = append(out, linesVersion(version, want))
		}
	}
	if !found && want != nil {
		out = append(out, linesVersion(version, want))
		sortVersionsDesc(out)
	}
	return out
}

func linesVersion(version string, lines []string) provider.ChangelogVersion {
	v := provider.ChangelogVersion{Version: version, Entries: make([]provider.ChangelogEntry, len(lines))}
	for i, l := range lines {
		v.Entries[i] = provider.ChangelogEntry{Text: l}
	}
	return v
}
