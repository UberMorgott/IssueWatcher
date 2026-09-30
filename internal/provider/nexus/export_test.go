package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Errors a fake tool answers with.
var (
	ErrDisabled = errDisabled
	ErrNotFound = errNotFound
	ErrUnsure   = errWriteUnsure
)

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

func (b fakeBackend) searchMods(_ context.Context, u uploader, count, offset int) (modsResult, error) {
	args := map[string]any{"uploader": u.name, "include_adult": true, "count": count, "offset": offset}
	if u.id > 0 {
		args = map[string]any{"uploader_id": u.id, "include_adult": true, "count": count, "offset": offset}
	}
	var r modsResult
	err := b.t.call("search_mods", args, &r)
	return r, err
}

func (b fakeBackend) comments(_ context.Context, game string, mod, page int) (commentsResult, error) {
	var r commentsResult
	err := b.t.call("get_mod_comments", map[string]any{"game": game, "mod_id": mod, "page": page}, &r)
	return r, err
}

func (b fakeBackend) bugs(_ context.Context, game string, mod, page int) (bugsResult, error) {
	var r bugsResult
	err := b.t.call("get_mod_bugs", map[string]any{"game": game, "mod_id": mod, "page": page}, &r)
	return r, err
}

func (b fakeBackend) bug(_ context.Context, issue int) (bugResult, error) {
	var r bugResult
	err := b.t.call("get_mod_bug", map[string]any{"issue_id": issue}, &r)
	return r, err
}

func (b fakeBackend) postComment(_ context.Context, game string, mod int, text string, parent int) (postResult, error) {
	var r postResult
	err := b.t.call("post_mod_comment", map[string]any{"game": game, "mod_id": mod, "text": text, "parent_id": parent}, &r)
	return r, err
}

func (b fakeBackend) replyBug(_ context.Context, issue int, text string) (postResult, error) {
	var r postResult
	err := b.t.call("reply_mod_bug", map[string]any{"issue_id": issue, "text": text}, &r)
	return r, err
}

// fakeSession is the fake's sign-in answer.
type fakeSession struct {
	LoggedIn     bool   `json:"loggedIn"`
	InProgress   bool   `json:"loginInProgress"`
	WindowOpened bool   `json:"loginWindowOpened"`
	Detail       string `json:"detail"`
	Account      *struct {
		MemberID int    `json:"memberId"`
		Name     string `json:"name"`
	} `json:"account"`
}

func (b fakeBackend) session(tool string) (provider.Login, *uploader, error) {
	var s fakeSession
	if err := b.t.call(tool, nil, &s); err != nil {
		return provider.Login{}, nil, err
	}
	l := provider.Login{LoggedIn: s.LoggedIn, InProgress: s.InProgress, Window: s.WindowOpened, Detail: s.Detail}
	var m *uploader
	if s.LoggedIn && s.Account != nil && s.Account.MemberID > 0 {
		m = &uploader{id: s.Account.MemberID, name: s.Account.Name}
		l.Account = s.Account.Name
	}
	return l, m, nil
}

func (b fakeBackend) login(ctx context.Context) (provider.Login, *uploader, error) {
	l, m, err := b.session("web_login")
	if err == nil && l.LoggedIn && l.Account == "" {
		return b.loginStatus(ctx) // web_login does not report the member
	}
	return l, m, err
}

func (b fakeBackend) loginStatus(context.Context) (provider.Login, *uploader, error) {
	return b.session("web_status")
}

func (b fakeBackend) logout(context.Context) error { return b.t.call("web_logout", nil, nil) }
func (b fakeBackend) cancelLogin(context.Context) error {
	return b.t.call("web_login_cancel", nil, nil)
}
