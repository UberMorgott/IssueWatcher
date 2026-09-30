package nexus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
)

// errDisabled: the mod has no Posts / Bugs tab.
var errDisabled = errors.New("nexus: tab disabled")

// errNotFound: the bug report is gone.
var errNotFound = errors.New("nexus: not found")

// errWriteUnsure marks a write that was sent without a clear answer: it may
// have been saved (the caller reads back, never re-sends).
var errWriteUnsure = errors.New("nexus: write outcome unknown")

// backend is how the provider reaches the site: the MCP server (old engine)
// or natively (browser + plain HTTP).
type backend interface {
	searchMods(ctx context.Context, u uploader, count, offset int) (modsResult, error)
	comments(ctx context.Context, game string, mod, page int) (commentsResult, error)
	bugs(ctx context.Context, game string, mod, page int) (bugsResult, error)
	bug(ctx context.Context, issue int) (bugResult, error)
	postComment(ctx context.Context, game string, mod int, text string, parent int) (postResult, error)
	replyBug(ctx context.Context, issue int, text string) (postResult, error)
	// sigKey is the poll_state key of the page-1 fingerprint (versioned per engine).
	sigKey() string
	login(ctx context.Context) (provider.Login, *uploader, error)
	loginStatus(ctx context.Context) (provider.Login, *uploader, error)
	logout(ctx context.Context) error
	cancelLogin(ctx context.Context) error
}

// mcpBackend is the old engine: the owner's nexusmods-mcp-server.
type mcpBackend struct {
	bridge Caller
	now    func() time.Time
}

func (b *mcpBackend) call(ctx context.Context, tool string, args map[string]any, out any) error {
	err := mcpbridge.ProviderError(b.bridge.Call(ctx, tool, args, out, true), b.now())
	switch {
	case mcpbridge.IsCode(err, mcpbridge.CodeDisabled):
		return fmt.Errorf("%w: %w", errDisabled, err)
	case mcpbridge.IsCode(err, mcpbridge.CodeNotFound):
		return fmt.Errorf("%w: %w", errNotFound, err)
	}
	return err
}

func (b *mcpBackend) sigKey() string { return "nexus:page1" }

func (b *mcpBackend) searchMods(ctx context.Context, u uploader, count, offset int) (modsResult, error) {
	args := u.args()
	args["count"] = count
	if offset > 0 || count > 1 {
		args["offset"] = offset
	}
	var r modsResult
	err := b.call(ctx, "search_mods", args, &r)
	return r, err
}

func (b *mcpBackend) comments(ctx context.Context, game string, mod, page int) (commentsResult, error) {
	var r commentsResult
	err := b.call(ctx, "get_mod_comments", map[string]any{"game": game, "mod_id": mod, "page": page}, &r)
	return r, err
}

func (b *mcpBackend) bugs(ctx context.Context, game string, mod, page int) (bugsResult, error) {
	var r bugsResult
	err := b.call(ctx, "get_mod_bugs", map[string]any{"game": game, "mod_id": mod, "page": page}, &r)
	return r, err
}

func (b *mcpBackend) bug(ctx context.Context, issue int) (bugResult, error) {
	var r bugResult
	err := b.call(ctx, "get_mod_bug", map[string]any{"issue_id": issue}, &r)
	return r, err
}

func (b *mcpBackend) write(ctx context.Context, tool string, args map[string]any) (postResult, error) {
	var r postResult
	err := b.bridge.Call(ctx, tool, args, &r, false)
	switch {
	case err == nil:
		return r, nil
	case mcpbridge.WriteUnsure(err):
		return r, fmt.Errorf("%w: %w", errWriteUnsure, err)
	}
	return r, mcpbridge.ProviderError(err, b.now())
}

func (b *mcpBackend) postComment(ctx context.Context, game string, mod int, text string, parent int) (postResult, error) {
	return b.write(ctx, "post_mod_comment", map[string]any{"game": game, "mod_id": mod, "text": text, "parent_id": parent})
}

func (b *mcpBackend) replyBug(ctx context.Context, issue int, text string) (postResult, error) {
	return b.write(ctx, "reply_mod_bug", map[string]any{"issue_id": issue, "text": text})
}

func (b *mcpBackend) session(s sessionResult) (provider.Login, *uploader) {
	l := provider.Login{LoggedIn: s.LoggedIn, InProgress: s.InProgress, Window: s.WindowOpened, Detail: cmpName(s.AccountError, s.Detail)}
	if s.LoggedIn {
		l.Source, l.Browser = deref(s.Source), deref(s.Browser)
	} else if s.InProgress || s.WindowOpened {
		l.Via, l.ViaBrowser = deref(s.Via), deref(s.ViaBrowser)
	}
	var m *uploader
	if s.LoggedIn && s.Account != nil && s.Account.MemberID > 0 {
		m = &uploader{id: s.Account.MemberID, name: s.Account.Name}
		l.Account = cmpName(s.Account.Name, fmt.Sprint(s.Account.MemberID))
	}
	return l, m
}

func (b *mcpBackend) login(ctx context.Context) (provider.Login, *uploader, error) {
	var s sessionResult
	if err := b.call(ctx, "web_login", nil, &s); err != nil {
		return provider.Login{}, nil, err
	}
	l, m := b.session(s)
	if l.LoggedIn && l.Account == "" {
		return b.loginStatus(ctx) // web_login does not report the member
	}
	return l, m, nil
}

func (b *mcpBackend) loginStatus(ctx context.Context) (provider.Login, *uploader, error) {
	var s sessionResult
	if err := b.call(ctx, "web_status", nil, &s); err != nil {
		return provider.Login{}, nil, err
	}
	l, m := b.session(s)
	return l, m, nil
}

func (b *mcpBackend) logout(ctx context.Context) error {
	return mcpbridge.ProviderError(b.bridge.Call(ctx, "web_logout", nil, nil, false), b.now())
}

func (b *mcpBackend) cancelLogin(ctx context.Context) error {
	return mcpbridge.ProviderError(b.bridge.Call(ctx, "web_login_cancel", nil, nil, false), b.now())
}
