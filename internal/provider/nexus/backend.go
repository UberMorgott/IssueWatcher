package nexus

import (
	"context"
	"errors"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// errDisabled: the mod has no Posts / Bugs tab.
var errDisabled = errors.New("nexus: tab disabled")

// errNotFound: the bug report is gone.
var errNotFound = errors.New("nexus: not found")

// errWriteUnsure marks a write that was sent without a clear answer: it may
// have been saved (the caller reads back, never re-sends).
var errWriteUnsure = errors.New("nexus: write outcome unknown")

// backend is how the provider reaches the site: the native engine (browser +
// plain HTTP); tests swap in a fake.
type backend interface {
	searchMods(ctx context.Context, u uploader, count, offset int) (modsResult, error)
	comments(ctx context.Context, game string, mod, page int) (commentsResult, error)
	bugs(ctx context.Context, game string, mod, page int) (bugsResult, error)
	bug(ctx context.Context, issue int) (bugResult, error)
	postComment(ctx context.Context, game string, mod int, text string, parent int) (postResult, error)
	replyBug(ctx context.Context, issue int, text string) (postResult, error)
	login(ctx context.Context) (provider.Login, *uploader, error)
	loginStatus(ctx context.Context) (provider.Login, *uploader, error)
	logout(ctx context.Context) error
	cancelLogin(ctx context.Context) error
}
