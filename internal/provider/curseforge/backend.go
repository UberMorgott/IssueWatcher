package curseforge

import (
	"context"
	"errors"

	"github.com/UberMorgott/issuewatcher/internal/provider"
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

// backend is how the provider reaches CurseForge: the native engine (plain
// HTTP, the browser only when a write needs it); tests swap in a fake.
type backend interface {
	// status is the session (Account / LoginStatus).
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
}
