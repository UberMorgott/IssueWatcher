package curseforge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
)

// errWriteUnsure marks a write sent without a clear answer (read back, never
// re-sent).
var errWriteUnsure = errors.New("curseforge: write outcome unknown")

type authorResult struct {
	Projects []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"projects"`
}

type projectResult struct {
	URL     *string `json:"url"`
	Summary string  `json:"summary"`
}

type postResult struct {
	Posted bool    `json:"posted"`
	ID     *string `json:"id"`
}

// backend is how the provider reaches CurseForge: the MCP server (old engine)
// or natively (plain HTTP, the browser only when a write needs it).
type backend interface {
	// status is the session (Account / LoginStatus); read errors mapped as call.
	status(ctx context.Context) (sessionResult, error)
	login(ctx context.Context) (provider.Login, error)
	loginStatus(ctx context.Context) (provider.Login, error)
	logout(ctx context.Context) error
	cancelLogin(ctx context.Context) error
	author(ctx context.Context, name string) (authorResult, error)
	project(ctx context.Context, id int) (projectResult, error)
	page(ctx context.Context, mod, page int) (commentsResult, error)
	// post sends one comment (RawHtml); errWriteUnsure = maybe saved.
	post(ctx context.Context, mod int, html string, parent int) (postResult, error)
	sigKey() string
	// v2 fingerprints also carry the entries' modification times.
	v2() bool
	scheduling() provider.Scheduling
}

type mcpBackend struct {
	bridge Caller
	now    func() time.Time
}

// call runs a read and maps session problems: not_logged_in / cloudflare →
// ErrRelogin (an error state, never "no new comments").
func (b *mcpBackend) call(ctx context.Context, tool string, args map[string]any, out any) error {
	err := b.bridge.Call(ctx, tool, args, out, true)
	if mcpbridge.IsCode(err, mcpbridge.CodeNotLoggedIn) || mcpbridge.IsCode(err, mcpbridge.CodeCloudflare) {
		return fmt.Errorf("%w: %w", ErrRelogin, err)
	}
	return mcpbridge.ProviderError(err, b.now())
}

func (b *mcpBackend) sigKey() string { return "curseforge:page1" }
func (b *mcpBackend) v2() bool       { return false }
func (b *mcpBackend) scheduling() provider.Scheduling {
	return provider.Scheduling{PollMinInterval: 15 * time.Minute, RateBudget: 60}
}

func (b *mcpBackend) status(ctx context.Context) (sessionResult, error) {
	var s sessionResult
	err := b.call(ctx, "cf_session_status", nil, &s)
	return s, err
}

func (b *mcpBackend) login(ctx context.Context) (provider.Login, error) {
	var r struct {
		Result       string  `json:"result"`
		LoggedIn     bool    `json:"loggedIn"`
		InProgress   bool    `json:"loginInProgress"`
		WindowOpened bool    `json:"loginWindowOpened"`
		Via          *string `json:"loginVia"`
		ViaBrowser   *string `json:"loginBrowser"`
	}
	if err := b.bridge.Call(ctx, "cf_auto_extract_cookies", nil, &r, true); err != nil {
		return provider.Login{}, mcpbridge.ProviderError(err, b.now())
	}
	if r.LoggedIn {
		return b.loginStatus(ctx)
	}
	return provider.Login{InProgress: r.InProgress || r.WindowOpened, Window: r.WindowOpened, Detail: r.Result,
		Via: deref(r.Via), ViaBrowser: deref(r.ViaBrowser)}, nil
}

func (b *mcpBackend) loginStatus(ctx context.Context) (provider.Login, error) {
	var s sessionResult
	if err := b.bridge.Call(ctx, "cf_session_status", nil, &s, true); err != nil {
		return provider.Login{}, mcpbridge.ProviderError(err, b.now())
	}
	l := provider.Login{LoggedIn: s.LoggedIn && s.name() != "", InProgress: s.InProgress, Detail: s.Detail}
	if !l.LoggedIn && s.InProgress {
		l.Via, l.ViaBrowser = deref(s.Via), deref(s.ViaBrowser)
	}
	if l.LoggedIn {
		l.Account = s.name()
		l.Source, l.Browser = deref(s.Source), deref(s.Browser)
	}
	return l, nil
}

func (b *mcpBackend) logout(ctx context.Context) error {
	return mcpbridge.ProviderError(b.bridge.Call(ctx, "cf_logout", nil, nil, false), b.now())
}

func (b *mcpBackend) cancelLogin(ctx context.Context) error {
	return mcpbridge.ProviderError(b.bridge.Call(ctx, "cf_login_cancel", nil, nil, false), b.now())
}

func (b *mcpBackend) author(ctx context.Context, name string) (authorResult, error) {
	var r authorResult
	err := b.call(ctx, "search_author", map[string]any{"username": name}, &r)
	return r, err
}

func (b *mcpBackend) project(ctx context.Context, id int) (projectResult, error) {
	var r projectResult
	err := b.call(ctx, "get_project", map[string]any{"project": fmt.Sprint(id)}, &r)
	return r, err
}

func (b *mcpBackend) page(ctx context.Context, mod, page int) (commentsResult, error) {
	var r commentsResult
	err := b.call(ctx, "get_comments", map[string]any{"mod_id": mod, "page": page}, &r)
	return r, err
}

func (b *mcpBackend) post(ctx context.Context, mod int, html string, parent int) (postResult, error) {
	var r postResult
	err := b.bridge.Call(ctx, "post_comment", map[string]any{"mod_id": mod, "comment_text": html, "reply_to_id": parent}, &r, false)
	switch {
	case err == nil:
		return r, nil
	case mcpbridge.IsCode(err, mcpbridge.CodeNotLoggedIn), mcpbridge.IsCode(err, mcpbridge.CodeCloudflare):
		return r, fmt.Errorf("%w: %w", provider.ErrNotSignedIn, err)
	case mcpbridge.WriteUnsure(err):
		return r, fmt.Errorf("%w: %w", errWriteUnsure, err)
	}
	return r, mcpbridge.ProviderError(err, b.now())
}
