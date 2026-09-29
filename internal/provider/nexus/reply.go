package nexus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
)

const canReply = true

// ErrUnknownOutcome: the post may or may not have landed and a read-back did
// not find it.
var ErrUnknownOutcome = errors.New("исход неизвестен — проверьте страницу")

type postResult struct {
	Posted   bool    `json:"posted"`
	ID       *string `json:"id"`
	Verified bool    `json:"verified"`
}

// readBackPages bounds the Posts pages searched for a lost reply.
const readBackPages = 5

// Reply implements provider.Provider: a reply inside the Posts thread
// (post_mod_comment{parent_id}) or on the bug report (reply_mod_bug). Never
// retried; a lost answer is resolved by reading the thread back.
func (p *Provider) Reply(ctx context.Context, itemExternalID, body string) (provider.Comment, error) {
	account, err := p.self(ctx)
	if err != nil {
		return provider.Comment{}, err
	}
	var (
		tool string
		args map[string]any
		url  string
		find func(context.Context) (string, error)
	)
	switch kind, rest, _ := strings.Cut(itemExternalID, ":"); kind {
	case "comment":
		game, mod, id, err := parseComment(rest)
		if err != nil {
			return provider.Comment{}, err
		}
		tool, url = "post_mod_comment", commentURL(game, mod, id)
		args = map[string]any{"game": game, "mod_id": mod, "text": body, "parent_id": mcpbridge.Number(id)}
		find = func(ctx context.Context) (string, error) { return p.findCommentReply(ctx, game, mod, id, account, body) }
	case "bug":
		tool = "reply_mod_bug"
		args = map[string]any{"issue_id": mcpbridge.Number(rest), "text": body}
		find = func(ctx context.Context) (string, error) { return p.findBugReply(ctx, rest, account, body) }
	default:
		return provider.Comment{}, fmt.Errorf("nexus: bad item id %q", itemExternalID)
	}
	var r postResult
	err = p.opts.Bridge.Call(ctx, tool, args, &r, false)
	switch {
	case err == nil && r.Posted && r.ID != nil && *r.ID != "":
		return p.posted(*r.ID, account, body, url), nil
	case err == nil && !r.Posted:
		return provider.Comment{}, fmt.Errorf("nexus: %s: not posted", tool)
	case err != nil && !errors.Is(err, mcpbridge.ErrOutcomeUnknown):
		return provider.Comment{}, mcpbridge.ProviderError(err, p.opts.Now())
	}
	// Posted without an id (the server's own read-back missed it) or the answer
	// was lost. A new post can take seconds to show up: look again a few times.
	var (
		id   string
		ferr error
	)
	for _, wait := range append([]time.Duration{0}, p.opts.ReadBackWaits...) {
		if !sleep(ctx, wait) {
			break
		}
		if id, ferr = find(ctx); ferr == nil && id != "" {
			break
		}
	}
	if ferr != nil || id == "" {
		p.opts.Log.Warn("nexus: reply outcome unknown", "item", itemExternalID, "err", err, "readback", ferr)
		return provider.Comment{}, fmt.Errorf("nexus: %w", ErrUnknownOutcome)
	}
	return p.posted(id, account, body, url), nil
}

// sleep waits d unless ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *Provider) posted(id, account, body, url string) provider.Comment {
	now := p.opts.Now().UTC()
	return provider.Comment{ExternalID: id, Author: account, Body: body, URL: url, CreatedAt: now, UpdatedAt: now}
}

// self is the account replies are posted as (comment author name).
func (p *Provider) self(ctx context.Context) (string, error) {
	p.mu.Lock()
	a := p.account
	p.mu.Unlock()
	if a != "" {
		return a, nil
	}
	return p.Account(ctx)
}

// parseComment splits "<game>/<mod>/<id>".
func parseComment(s string) (string, int, string, error) {
	proj, id, ok := strings.CutLast(s, "/")
	if !ok {
		return "", 0, "", fmt.Errorf("nexus: bad comment id %q", s)
	}
	game, mod, err := parseProject(proj)
	if err != nil {
		return "", 0, "", err
	}
	if _, err := strconv.Atoi(id); err != nil {
		return "", 0, "", fmt.Errorf("nexus: bad comment id %q", s)
	}
	return game, mod, id, nil
}

// newest returns the highest numeric id.
func newest(ids []string) string {
	best, bestN := "", -1
	for _, id := range ids {
		if n := mcpbridge.Number(id); n > bestN {
			best, bestN = id, n
		}
	}
	return best
}

func (p *Provider) findCommentReply(ctx context.Context, game string, mod int, parent, account, body string) (string, error) {
	var hits []string // the thread may show up twice while pages shift
	for page := 1; page <= readBackPages; page++ {
		var r commentsResult
		if err := p.call(ctx, "get_mod_comments", map[string]any{"game": game, "mod_id": mod, "page": page}, &r); err != nil {
			return "", err
		}
		for _, t := range r.Comments {
			if t.ID != parent {
				continue
			}
			for _, rp := range t.Replies {
				if rp.Author == account && mcpbridge.SameText(rp.Body, body) {
					hits = append(hits, rp.ID)
				}
			}
		}
		if len(hits) > 0 || page >= r.Pages {
			break
		}
	}
	return newest(hits), nil
}

func (p *Provider) findBugReply(ctx context.Context, issue, account, body string) (string, error) {
	var r bugResult
	if err := p.call(ctx, "get_mod_bug", map[string]any{"issue_id": mcpbridge.Number(issue)}, &r); err != nil {
		return "", err
	}
	var hits []string
	for _, rp := range r.Replies {
		if rp.Author == account && mcpbridge.SameText(rp.Body, body) {
			hits = append(hits, rp.ID)
		}
	}
	return newest(hits), nil
}
