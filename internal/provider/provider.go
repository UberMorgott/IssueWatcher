// Package provider defines the platform adapter contract (GitHub first) and
// the platform-neutral records adapters return.
package provider

import (
	"context"
	"errors"
	"time"
)

// AuthKind is how a provider signs the user in.
type AuthKind string

// Auth kinds (docs/ARCHITECTURE.md → Provider interface).
const (
	AuthOAuthLoopback AuthKind = "oauth-loopback"
	AuthDevice        AuthKind = "device"
	AuthSSOWebsocket  AuthKind = "sso-websocket"
	AuthCookieSession AuthKind = "cookie-session"
	AuthAPIKey        AuthKind = "api-key"
)

// Capabilities tells the UI which actions a provider supports.
type Capabilities struct {
	ListProjects bool     `json:"listProjects"`
	SyncItems    bool     `json:"syncItems"`
	ListComments bool     `json:"listComments"`
	Reply        bool     `json:"reply"`
	SetLabels    bool     `json:"setLabels"`
	SetStatus    bool     `json:"setStatus"`
	CreatePR     bool     `json:"createPR"`
	Auth         AuthKind `json:"auth"`
}

// ErrNotSignedIn means the provider has no usable credentials.
var ErrNotSignedIn = errors.New("provider: not signed in")

// RateLimitError means the platform quota is exhausted until Reset.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return "provider: rate limited until " + e.Reset.UTC().Format(time.RFC3339)
}

// Project is a repo / mod page.
type Project struct {
	ExternalID string // stable platform key, e.g. owner/repo
	Name       string
	URL        string
}

// Comment on an item.
type Comment struct {
	ExternalID string
	Author     string
	Body       string
	URL        string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Item is an issue (or platform equivalent) with all its comments.
type Item struct {
	ExternalID string // platform node id
	Kind       string // issue
	Number     int
	Title      string
	Body       string
	URL        string
	Author     string
	Open       bool
	RawStatus  string
	Labels     []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ClosedAt   time.Time // zero while open
	Comments   []Comment
}

// Provider is one platform adapter.
type Provider interface {
	Platform() string
	Capabilities() Capabilities
	// Account is the signed-in login; ErrNotSignedIn without credentials.
	Account(ctx context.Context) (string, error)
	ListProjects(ctx context.Context) ([]Project, error)
	// SyncItems returns items of project updated at or after since (zero = all),
	// oldest update first, each with its full comment list.
	SyncItems(ctx context.Context, project Project, since time.Time) ([]Item, error)
	// Reply posts a comment on the item identified by its ExternalID.
	Reply(ctx context.Context, itemExternalID, body string) (Comment, error)
}
