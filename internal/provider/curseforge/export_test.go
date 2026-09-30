package curseforge

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// ErrUnsure is what a fake write answers for "sent, outcome unknown".
var ErrUnsure = errWriteUnsure

// Tools is a fake backend for the provider-logic tests: named handlers answer
// JSON-shaped results, decoded into the backend's types.
type Tools struct {
	mu    sync.Mutex
	h     map[string]func(args map[string]any) (any, error)
	calls map[string]int
}

// NewTools creates an empty fake.
func NewTools() *Tools {
	return &Tools{h: map[string]func(map[string]any) (any, error){}, calls: map[string]int{}}
}

// Handle sets name's handler (replacing an earlier one).
func (t *Tools) Handle(name string, h func(args map[string]any) (any, error)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.h[name] = h
}

// Count returns how many calls name got.
func (t *Tools) Count(name string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls[name]
}

// NewWithTools is New over the fake.
func NewWithTools(opts Options, t *Tools) *Provider {
	p := New(opts)
	p.b = fakeBackend{t}
	return p
}

func (t *Tools) call(name string, args map[string]any, out any) error {
	j, _ := json.Marshal(args)
	a := map[string]any{}
	_ = json.Unmarshal(j, &a)
	t.mu.Lock()
	t.calls[name]++
	h := t.h[name]
	t.mu.Unlock()
	if h == nil {
		return fmt.Errorf("fake: no handler for %s", name)
	}
	v, err := h(a)
	if err != nil || out == nil {
		return err
	}
	if j, err = json.Marshal(v); err != nil {
		return err
	}
	return json.Unmarshal(j, out)
}

type fakeBackend struct{ t *Tools }

func (b fakeBackend) status(context.Context) (sessionResult, error) {
	var s sessionResult
	err := b.t.call("cf_session_status", nil, &s)
	return s, err
}

func (b fakeBackend) login(ctx context.Context) (provider.Login, error) {
	var r struct {
		Result       string `json:"result"`
		LoggedIn     bool   `json:"loggedIn"`
		InProgress   bool   `json:"loginInProgress"`
		WindowOpened bool   `json:"loginWindowOpened"`
	}
	if err := b.t.call("cf_auto_extract_cookies", nil, &r); err != nil {
		return provider.Login{}, err
	}
	if r.LoggedIn {
		return b.loginStatus(ctx)
	}
	return provider.Login{InProgress: r.InProgress || r.WindowOpened, Window: r.WindowOpened, Detail: r.Result}, nil
}

func (b fakeBackend) loginStatus(ctx context.Context) (provider.Login, error) {
	s, err := b.status(ctx)
	if err != nil {
		return provider.Login{}, err
	}
	l := provider.Login{LoggedIn: s.LoggedIn && s.name() != "", InProgress: s.InProgress, Detail: s.Detail}
	if l.LoggedIn {
		l.Account = s.name()
	}
	return l, nil
}

func (b fakeBackend) logout(context.Context) error      { return b.t.call("cf_logout", nil, nil) }
func (b fakeBackend) cancelLogin(context.Context) error { return b.t.call("cf_login_cancel", nil, nil) }

func (b fakeBackend) author(_ context.Context, name string) (authorResult, error) {
	var r authorResult
	err := b.t.call("search_author", map[string]any{"username": name}, &r)
	return r, err
}

func (b fakeBackend) project(_ context.Context, id int) (projectResult, error) {
	var r projectResult
	err := b.t.call("get_project", map[string]any{"project": fmt.Sprint(id)}, &r)
	return r, err
}

func (b fakeBackend) page(_ context.Context, mod, page int) (commentsResult, error) {
	var r commentsResult
	err := b.t.call("get_comments", map[string]any{"mod_id": mod, "page": page}, &r)
	return r, err
}

func (b fakeBackend) post(_ context.Context, mod int, html string, parent int) (postResult, error) {
	var r postResult
	err := b.t.call("post_comment", map[string]any{"mod_id": mod, "comment_text": html, "reply_to_id": parent}, &r)
	return r, err
}
