package nexus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/modkit"
)

const canReply = true

// ErrUnknownOutcome: the post may or may not have landed and a read-back did
// not find it.
var ErrUnknownOutcome = modkit.ErrUnknownOutcome

type postResult struct {
	Posted   bool    `json:"posted"`
	ID       *string `json:"id"`
	Verified bool    `json:"verified"`
}

// readBackPages bounds the Posts pages searched for a lost reply.
const readBackPages = 5

// readBackTimeout bounds the read-back of a reply whose outcome is unknown; it
// runs even when the request that posted it was cancelled.
const readBackTimeout = 2 * time.Minute

// readBackSkew is how much older than the send time a found reply may look
// (clock differences) and still count as the one just posted.
const readBackSkew = 5 * time.Minute

// match is the read-back filter: the account's reply with the same text that
// was not there before the post and (when the site gives a time) is not older
// than the send.
type match struct {
	account, body string
	before        map[string]bool
	sent          time.Time
}

// fresh: the account's reply that was not there before the post and is not
// older than the send.
func (m match) fresh(id, author string, created time.Time) bool {
	return !m.before[id] && author == m.account && (created.IsZero() || !created.Before(m.sent.Add(-readBackSkew)))
}

// ok: a fresh reply with the sent text. The site shows BBCode rendered
// ([url=…]GitHub Issues[/url] reads back as "GitHub Issues"), so the sent
// body is compared both as typed and with its tags dropped.
func (m match) ok(id, author, body string, created time.Time) bool {
	return m.fresh(id, author, created) && modkit.SameRendered(body, m.body)
}

// pick is the read-back verdict over the thread's replies: the newest fresh
// reply with the sent text, else the only fresh reply of the account (the
// thread is locked to one write at a time; rendering can change the text
// beyond a tag strip: images, quotes, smileys), else "" (unknown).
func (m match) pick(text, fresh []string) string {
	if id := newest(text); id != "" {
		return id
	}
	if len(fresh) == 1 {
		return fresh[0]
	}
	return ""
}

// Reply implements provider.Provider: a reply inside the Posts thread
// (post_mod_comment{parent_id}) or on the bug report (reply_mod_bug). Never
// retried; a lost or failed-after-send answer is resolved by reading the
// thread back, ignoring the replies that existed before the post.
func (p *Provider) Reply(ctx context.Context, itemExternalID, body string) (provider.Comment, error) {
	account, err := p.self(ctx)
	if err != nil {
		return provider.Comment{}, err
	}
	var (
		tool     string
		send     func(context.Context) (postResult, error)
		url      string
		snapshot func(context.Context) (map[string]bool, error)
		find     func(context.Context, match) (string, error)
	)
	switch kind, rest, _ := strings.Cut(itemExternalID, ":"); kind {
	case "comment":
		game, mod, id, err := parseComment(rest)
		if err != nil {
			return provider.Comment{}, err
		}
		tool, url = "post_mod_comment", commentURL(game, mod, id)
		send = func(ctx context.Context) (postResult, error) {
			return p.b.postComment(ctx, game, mod, body, modkit.Number(id))
		}
		snapshot = func(ctx context.Context) (map[string]bool, error) { return p.commentReplyIDs(ctx, game, mod, id) }
		find = func(ctx context.Context, m match) (string, error) { return p.findCommentReply(ctx, game, mod, id, m) }
	case "bug":
		tool = "reply_mod_bug"
		send = func(ctx context.Context) (postResult, error) { return p.b.replyBug(ctx, modkit.Number(rest), body) }
		snapshot = func(ctx context.Context) (map[string]bool, error) { return p.bugReplyIDs(ctx, rest) }
		find = func(ctx context.Context, m match) (string, error) { return p.findBugReply(ctx, rest, m) }
	default:
		return provider.Comment{}, fmt.Errorf("nexus: bad item id %q", itemExternalID)
	}
	// One write at a time per thread.
	lock, _ := p.threads.LoadOrStore(itemExternalID, &sync.Mutex{})
	mu, _ := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	// The replies already there: an older reply with the same text is never
	// taken for this one. A failed read sends nothing.
	before, err := snapshot(ctx)
	if err != nil {
		return provider.Comment{}, fmt.Errorf("nexus: read thread before reply: %w", err)
	}
	m := match{account: account, body: body, before: before, sent: p.opts.Now()}
	r, err := send(ctx)
	switch {
	case err == nil && r.Posted && r.ID != nil && *r.ID != "":
		return p.posted(*r.ID, account, body, url), nil
	case err == nil && !r.Posted:
		return provider.Comment{}, fmt.Errorf("nexus: %s: not posted", tool)
	case err != nil && !errors.Is(err, errWriteUnsure):
		return provider.Comment{}, err
	}
	// Posted without an id (the engine's own read-back missed it), the answer
	// was lost, or the site failed after the post was sent (it may have saved
	// it). A new post can take seconds to show up: look again a few times.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBackTimeout)
	defer cancel()
	var (
		id   string
		ferr error
	)
	for _, wait := range append([]time.Duration{0}, p.opts.ReadBackWaits...) {
		if !sleep(rctx, wait) {
			break
		}
		if id, ferr = find(rctx, m); ferr == nil && id != "" {
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
		if n := modkit.Number(id); n > bestN {
			best, bestN = id, n
		}
	}
	return best
}

// threadReplies calls fn with the replies of thread parent on each of the
// first readBackPages Posts pages that list it, until fn returns true.
func (p *Provider) threadReplies(ctx context.Context, game string, mod int, parent string, fn func([]post) bool) error {
	for page := 1; page <= readBackPages; page++ {
		r, err := p.b.comments(ctx, game, mod, page)
		if err != nil {
			return err
		}
		for _, t := range r.Comments {
			if t.ID == parent && fn(t.Replies) {
				return nil
			}
		}
		if page >= r.Pages {
			return nil
		}
	}
	return nil
}

func (p *Provider) commentReplyIDs(ctx context.Context, game string, mod int, parent string) (map[string]bool, error) {
	ids := map[string]bool{}
	err := p.threadReplies(ctx, game, mod, parent, func(rs []post) bool {
		for _, rp := range rs {
			ids[rp.ID] = true
		}
		return true
	})
	return ids, err
}

func (p *Provider) findCommentReply(ctx context.Context, game string, mod int, parent string, m match) (string, error) {
	// The thread may show up twice while pages shift: ids are deduplicated.
	var hits, fresh []string
	seen := map[string]bool{}
	err := p.threadReplies(ctx, game, mod, parent, func(rs []post) bool {
		for _, rp := range rs {
			created := modkit.Time(rp.CreatedAt)
			if seen[rp.ID] || !m.fresh(rp.ID, rp.Author, created) {
				continue
			}
			seen[rp.ID] = true
			fresh = append(fresh, rp.ID)
			if m.ok(rp.ID, rp.Author, rp.Body, created) {
				hits = append(hits, rp.ID)
			}
		}
		return len(hits) > 0
	})
	if err != nil {
		return "", err
	}
	return m.pick(hits, fresh), nil
}

func (p *Provider) bugReplies(ctx context.Context, issue string) ([]bugPost, error) {
	r, err := p.b.bug(ctx, modkit.Number(issue))
	if err != nil {
		return nil, err
	}
	return r.Replies, nil
}

func (p *Provider) bugReplyIDs(ctx context.Context, issue string) (map[string]bool, error) {
	rs, err := p.bugReplies(ctx, issue)
	ids := map[string]bool{}
	for _, rp := range rs {
		ids[rp.ID] = true
	}
	return ids, err
}

// findBugReply: bug posts carry only a profile-local time (its zone may not be
// this machine's), so only the pre-post snapshot tells old from new.
func (p *Provider) findBugReply(ctx context.Context, issue string, m match) (string, error) {
	rs, err := p.bugReplies(ctx, issue)
	if err != nil {
		return "", err
	}
	var hits, fresh []string
	for _, rp := range rs {
		if !m.fresh(rp.ID, rp.Author, time.Time{}) {
			continue
		}
		fresh = append(fresh, rp.ID)
		if m.ok(rp.ID, rp.Author, rp.Body, time.Time{}) {
			hits = append(hits, rp.ID)
		}
	}
	return m.pick(hits, fresh), nil
}
