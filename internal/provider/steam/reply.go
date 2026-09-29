package steam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// ErrSessionExpired: Steam refused the stored web cookies; the user must sign
// in again (Settings › Платформы › Steam). It is a provider.ErrNotSignedIn.
var ErrSessionExpired = fmt.Errorf("%w: Steam session expired — sign in again", provider.ErrNotSignedIn)

// ErrOutcomeUnknown: the post may or may not have landed and a read-back did
// not find it. Never retried automatically (a retry could post twice).
var ErrOutcomeUnknown = errors.New("steam: outcome unknown — check the item page before retrying")

func parseItemExternalID(s string) (fileID, commentID string, ok bool) {
	rest, ok := strings.CutPrefix(s, "comment:")
	if !ok {
		return "", "", false
	}
	fileID, commentID, ok = strings.Cut(rest, "/")
	return fileID, commentID, ok && isDigits(fileID) && isDigits(commentID)
}

// postAnswer is the post endpoint's JSON (the render shape plus an error text).
type postAnswer struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	HTML    string `json:"comments_html"`
}

// Reply implements provider.Provider. Steam comments are flat, so the reply is
// a new top-level comment on the Workshop item that starts with @<author> of
// the comment replied to (unless the body already starts with '@'). Posting
// needs the user's steamLoginSecure + sessionid cookies; a refusal marks the
// session expired. Never retried.
func (p *Provider) Reply(ctx context.Context, itemExternalID, body string) (provider.Comment, error) {
	fileID, commentID, ok := parseItemExternalID(itemExternalID)
	if !ok {
		return provider.Comment{}, fmt.Errorf("steam: not a comment item: %q", itemExternalID)
	}
	s, err := p.current()
	if err != nil {
		return provider.Comment{}, err
	}
	if s.SteamID == "" || s.LoginSecure == "" || s.SessionID == "" {
		return provider.Comment{}, fmt.Errorf("%w: sign in to Steam to post", provider.ErrNotSignedIn)
	}
	if s, err = p.ensureFresh(ctx, s); err != nil {
		return provider.Comment{}, err
	}
	if s.SessionExpired {
		return provider.Comment{}, ErrSessionExpired
	}
	c, err := p.replyOnce(ctx, s, fileID, commentID, body)
	if errors.Is(err, ErrSessionExpired) && s.RefreshToken != "" {
		// Refused before posting: renew the web session once and post again.
		s2, rerr := p.refresh(ctx, s)
		if rerr != nil {
			return provider.Comment{}, err
		}
		return p.replyOnce(ctx, s2, fileID, commentID, body)
	}
	return c, err
}

// replyOnce posts with the session s; a refusal marks s expired.
func (p *Provider) replyOnce(ctx context.Context, s Settings, fileID, commentID, body string) (provider.Comment, error) {
	creator, err := p.creator(ctx, fileID)
	if err != nil {
		return provider.Comment{}, err
	}
	target, err := p.findComment(ctx, creator, fileID, commentID)
	if err != nil {
		return provider.Comment{}, err
	}
	text := strings.TrimSpace(body)
	if !strings.HasPrefix(text, "@") && target.SteamID != s.SteamID && target.Author != "" {
		text = "@" + target.Author + " " + text
	}
	form := url.Values{
		"comment":   {text},
		"count":     {"10"},
		"sessionid": {s.SessionID},
		"feature2":  {"-1"},
	}
	cookies := "sessionid=" + s.SessionID + "; steamLoginSecure=" + s.LoginSecure // validated tokens: no ; or spaces
	sent := p.opts.Now()
	u := p.opts.CommunityURL + fmt.Sprintf(p.threadPath(creator, fileID), "post")
	a, err := p.postForm(ctx, u, form, cookies)
	var rl *provider.RateLimitError
	switch {
	case errors.As(err, &rl):
		return provider.Comment{}, err // refused before posting
	case err != nil:
		// The request may have reached Steam: look for it instead of retrying.
		p.opts.Log.Warn("steam: post failed, reading back", "item", fileID, "err", err)
		return p.readBack(context.WithoutCancel(ctx), creator, fileID, s, text, sent)
	case a.Status == http.StatusUnauthorized || a.Status == http.StatusForbidden || (a.URL != nil && strings.Contains(a.URL.Path, "/login")):
		return provider.Comment{}, p.expire(s)
	case a.Status != http.StatusOK:
		return provider.Comment{}, fmt.Errorf("steam: post comment: HTTP %d", a.Status)
	}
	var r postAnswer
	if err := json.Unmarshal(a.Body, &r); err != nil {
		if strings.Contains(strings.ToLower(string(a.Body)), "login") { // an HTML sign-in page
			return provider.Comment{}, p.expire(s)
		}
		return provider.Comment{}, fmt.Errorf("steam: post comment: %w", err)
	}
	if !r.Success {
		if notLoggedIn(r.Error) {
			return provider.Comment{}, p.expire(s)
		}
		return provider.Comment{}, fmt.Errorf("steam: post comment refused: %s", cmpStr(r.Error, "success=false"))
	}
	p.sessionOK(s)
	if cs, err := parseComments(r.HTML); err == nil {
		if c, ok := match(cs, s.SteamID, text, sent); ok {
			return p.comment(fileID, c, s.SteamID), nil
		}
	}
	return p.readBack(ctx, creator, fileID, s, text, sent)
}

// findComment reads the thread until comment id (the @author of the reply).
func (p *Provider) findComment(ctx context.Context, creator, fileID, id string) (comment, error) {
	for start, page := 0, 0; page < maxPages; page++ {
		r, err := p.render(ctx, creator, fileID, start, p.opts.PageSize)
		if err != nil {
			return comment{}, err
		}
		cs, err := parseComments(r.HTML)
		if err != nil {
			return comment{}, err
		}
		for _, c := range cs {
			if c.ID == id {
				return c, nil
			}
		}
		start += len(cs)
		if len(cs) == 0 || start >= r.TotalCount {
			break
		}
	}
	return comment{}, fmt.Errorf("steam: comment %s not found (deleted?)", id)
}

// readBack looks for the just-posted comment on the thread's first page.
func (p *Provider) readBack(ctx context.Context, creator, fileID string, s Settings, text string, sent time.Time) (provider.Comment, error) {
	self := s.SteamID
	r, err := p.render(ctx, creator, fileID, 0, 10)
	if err != nil {
		return provider.Comment{}, fmt.Errorf("%w (%w)", ErrOutcomeUnknown, err)
	}
	cs, err := parseComments(r.HTML)
	if err != nil {
		return provider.Comment{}, fmt.Errorf("%w (%w)", ErrOutcomeUnknown, err)
	}
	if c, ok := match(cs, self, text, sent); ok {
		p.sessionOK(s)
		return p.comment(fileID, c, self), nil
	}
	return provider.Comment{}, ErrOutcomeUnknown
}

// match finds the owner's comment with text posted around sent.
func match(cs []comment, self, text string, sent time.Time) (comment, bool) {
	want := strings.Join(strings.Fields(text), " ")
	for _, c := range cs {
		if c.SteamID == self && strings.Join(strings.Fields(c.Body), " ") == want && !c.CreatedAt.Before(sent.Add(-2*time.Minute)) {
			return c, true
		}
	}
	return comment{}, false
}

func (p *Provider) comment(fileID string, c comment, self string) provider.Comment {
	return provider.Comment{
		ExternalID: c.ID, Author: self, Body: c.Body,
		URL:       p.fileURL(fileID) + "#comment_" + c.ID,
		CreatedAt: c.CreatedAt, UpdatedAt: c.CreatedAt,
	}
}

func notLoggedIn(msg string) bool {
	m := strings.ToLower(msg)
	for _, k := range []string{"logged in", "log in", "sign in", "signed in", "session"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// expire marks the cookies used (seen) refused and returns ErrSessionExpired.
func (p *Provider) expire(seen Settings) error {
	p.setSession(seen, true)
	return ErrSessionExpired
}

func (p *Provider) sessionOK(seen Settings) { p.setSession(seen, false) }

// setSession records the session state of the cookies the request used. New
// cookies pasted meanwhile are left alone (neither flagged by an old refusal
// nor overwritten on disk by the old ones).
func (p *Provider) setSession(seen Settings, expired bool) {
	p.save.Lock()
	defer p.save.Unlock()
	p.mu.Lock()
	s := p.settings
	if s.LoginSecure != seen.LoginSecure || s.SessionID != seen.SessionID {
		p.mu.Unlock()
		return
	}
	s.SessionExpired, s.Verified, s.CheckedAt = expired, !expired, p.opts.Now().UTC()
	p.settings = s
	p.mu.Unlock()
	if err := saveSettings(p.opts.Dir, s); err != nil {
		p.opts.Log.Error("steam: save session state", "err", err)
	}
	if expired {
		p.opts.Log.Warn("steam: session expired; new cookies needed", "checked", strconv.FormatInt(s.CheckedAt.Unix(), 10))
	}
}
