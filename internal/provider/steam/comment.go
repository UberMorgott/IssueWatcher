package steam

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// CommentResult is a new top-level comment on a Workshop item: the plan of a
// dry run, or the plan plus the posted comment.
type CommentResult struct {
	Item       string            `json:"item"`
	Creator    string            `json:"creator"` // the item creator's SteamID64 (the thread is keyed by it)
	Endpoint   string            `json:"endpoint"`
	Form       []string          `json:"form"` // the form field names sent (values: the body, the stored sessionid)
	BodyLength int               `json:"bodyLength"`
	Body       string            `json:"body"`
	SignedIn   bool              `json:"signedIn"` // the stored web session (cookies) is present and not expired
	DryRun     bool              `json:"dryRun"`
	URL        string            `json:"url"` // the comment's permalink once posted, else the item page
	Comment    *provider.Comment `json:"comment,omitempty"`
}

// PostComment posts body as a new top-level comment on Workshop item fileID
// as the signed-in user (not a reply: no @author is added). dryRun resolves
// the item's creator and endpoint and posts nothing. Like Reply, a post that
// may have landed is never retried (ErrOutcomeUnknown).
func (p *Provider) PostComment(ctx context.Context, fileID, body string, dryRun bool) (CommentResult, error) {
	if !isDigits(fileID) {
		return CommentResult{}, fmt.Errorf("steam: bad Workshop item id %q", fileID)
	}
	text := strings.TrimSpace(body)
	if text == "" {
		return CommentResult{}, fmt.Errorf("steam: empty comment")
	}
	if utf8.RuneCountInString(text) > MaxReply {
		return CommentResult{}, fmt.Errorf("%w (%d characters, Steam takes at most %d)", provider.ErrReplyTooLong, utf8.RuneCountInString(text), MaxReply)
	}
	creator, err := p.creator(ctx, fileID)
	if err != nil {
		return CommentResult{}, err
	}
	s, err := p.current()
	if err != nil {
		return CommentResult{}, err
	}
	r := CommentResult{
		Item: fileID, Creator: creator, Endpoint: p.postURL(creator, fileID),
		Form: []string{"comment", "count", "feature2", "sessionid"}, BodyLength: utf8.RuneCountInString(text), Body: text,
		SignedIn: s.SteamID != "" && s.LoginSecure != "" && s.SessionID != "" && !s.SessionExpired,
		DryRun:   dryRun, URL: p.fileURL(fileID),
	}
	if dryRun {
		return r, nil
	}
	c, err := p.withSession(ctx, func(s Settings) (provider.Comment, error) {
		return p.postOnce(ctx, s, creator, fileID, text)
	})
	if err != nil {
		return CommentResult{}, err
	}
	r.Comment, r.URL = &c, c.URL
	return r, nil
}
