// Package signin is the generic «Подключить» of the native mod platforms
// (docs/ARCHITECTURE.md → Native mod platforms): (1) the stored session is
// valid → connected silently; (2) our browser profile is already signed in
// (headless) → its cookies are captured; (3) else a visible window of the
// installed browser shows the platform's sign-in page, polled every 2 s up to
// 10 min, captured, closed. «Выйти» clears the stored session and the
// profile's cookies/storage of that platform, so no silent capture undoes it.
package signin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// Browser is the part of *browser.Browser a sign-in uses.
type Browser interface {
	Cookies(ctx context.Context, domains []string) ([]browser.Cookie, error)
	OpenWindow(ctx context.Context, u string) error
	WindowOpen(ctx context.Context) bool
	ClearSite(ctx context.Context, domains, origins []string) error
	UserAgent() string
	Name() string
	Stop()
}

// Probe checks a candidate session and returns its account; an error that
// is provider.ErrNotSignedIn means "not signed in (yet)".
type Probe func(ctx context.Context, jar *websession.Jar) (string, error)

// Spec is one platform's sign-in.
type Spec struct {
	Platform string
	LoginURL string
	Domains  []string // cookie domains captured and cleared
	Origins  []string // storage cleared on «Выйти»
	Probe    Probe
	Interval time.Duration // default 2 s
	Timeout  time.Duration // default 10 min
	Log      *slog.Logger
}

// Session sources (provider.Login.Source).
const (
	SourceWindow  = "window"
	SourceProfile = "profile"
	SourceStored  = "stored"
)

// Manager runs one platform's sign-ins; transitions are serialized.
type Manager struct {
	spec Spec
	jar  *websession.Jar
	br   Browser

	mu      sync.Mutex
	waiting bool
	cancel  context.CancelFunc
	detail  string // why the last window sign-in ended without a session
	done    chan struct{}
}

// New creates the manager of spec over jar and the shared browser.
func New(spec Spec, jar *websession.Jar, br Browser) *Manager {
	if spec.Interval <= 0 {
		spec.Interval = 2 * time.Second
	}
	if spec.Timeout <= 0 {
		spec.Timeout = 10 * time.Minute
	}
	if spec.Log == nil {
		spec.Log = slog.New(slog.DiscardHandler)
	}
	return &Manager{spec: spec, jar: jar, br: br}
}

// Jar is the stored session.
func (m *Manager) Jar() *websession.Jar { return m.jar }

// Login implements the three-step «Подключить»; a window sign-in returns
// at once with InProgress (Status reports its end).
func (m *Manager) Login(ctx context.Context) (provider.Login, error) {
	m.mu.Lock()
	if m.waiting {
		m.mu.Unlock()
		return m.windowState(), nil
	}
	m.detail = ""
	m.mu.Unlock()
	// (1) the stored session.
	if !m.jar.Empty() {
		acc, err := m.spec.Probe(ctx, m.jar)
		switch {
		case err == nil:
			_ = m.jar.SetAccount(acc)
			return provider.Login{LoggedIn: true, Account: acc, Source: cmp(m.jar.Source(), SourceStored)}, nil
		case !errors.Is(err, provider.ErrNotSignedIn):
			return provider.Login{}, err
		}
	}
	// (2) our profile is already signed in.
	if acc, ok, err := m.capture(ctx, SourceProfile); err != nil {
		return provider.Login{}, err
	} else if ok {
		return provider.Login{LoggedIn: true, Account: acc, Source: SourceProfile, Browser: m.br.Name()}, nil
	}
	// (3) the visible sign-in window.
	m.br.Stop() // one profile: headless ends before the window opens
	if err := m.br.OpenWindow(ctx, m.spec.LoginURL); err != nil {
		return provider.Login{}, fmt.Errorf("%s: open the sign-in window: %w", m.spec.Platform, err)
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.spec.Timeout)
	done := make(chan struct{})
	m.mu.Lock()
	m.waiting, m.cancel, m.done = true, cancel, done
	m.mu.Unlock()
	m.spec.Log.Info("sign-in window opened", "platform", m.spec.Platform, "browser", m.br.Name())
	go m.wait(wctx, cancel, done)
	l := m.windowState()
	l.Window = true
	return l, nil
}

func (m *Manager) windowState() provider.Login {
	return provider.Login{InProgress: true, Via: "window", ViaBrowser: m.br.Name(), Detail: "Войдите в открывшемся окне браузера"}
}

// capture probes the profile's cookies and stores them when signed in.
func (m *Manager) capture(ctx context.Context, source string) (string, bool, error) {
	cks, err := m.br.Cookies(ctx, m.spec.Domains)
	if err != nil {
		return "", false, err
	}
	if len(cks) == 0 {
		return "", false, nil
	}
	cand := websession.Memory(websession.FromBrowser(cks), m.br.UserAgent())
	acc, err := m.spec.Probe(ctx, cand)
	switch {
	case errors.Is(err, provider.ErrNotSignedIn):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	if err := m.jar.Replace(cand.Cookies(), cand.UserAgent(), acc, source); err != nil {
		return "", false, err
	}
	m.spec.Log.Info("session captured", "platform", m.spec.Platform, "source", source, "account", acc)
	return acc, true, nil
}

func (m *Manager) wait(ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	defer close(done)
	defer cancel()
	detail := ""
	defer func() {
		m.br.Stop() // the window closes; reads start headless again
		m.mu.Lock()
		m.waiting, m.cancel, m.detail = false, nil, detail
		m.mu.Unlock()
	}()
	t := time.NewTicker(m.spec.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				detail = "время входа истекло"
			} else {
				detail = "вход отменён"
			}
			return
		case <-t.C:
		}
		if !m.br.WindowOpen(ctx) {
			detail = "окно входа закрыто"
			return
		}
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		_, ok, err := m.capture(pctx, SourceWindow)
		pcancel()
		if err != nil {
			m.spec.Log.Debug("sign-in probe failed", "platform", m.spec.Platform, "err", err)
		}
		if ok {
			return
		}
	}
}

// Status is the stored session (no network) or the window sign-in in progress.
func (m *Manager) Status() provider.Login {
	m.mu.Lock()
	waiting, detail := m.waiting, m.detail
	m.mu.Unlock()
	if waiting {
		return m.windowState()
	}
	if !m.jar.Empty() && m.jar.Account() != "" {
		return provider.Login{LoggedIn: true, Account: m.jar.Account(), Source: cmp(m.jar.Source(), SourceStored)}
	}
	return provider.Login{Detail: detail}
}

// Cancel stops a waiting window sign-in (the window closes).
func (m *Manager) Cancel() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Logout clears the stored session and the profile's cookies and storage of
// the platform.
func (m *Manager) Logout(ctx context.Context) error {
	m.Cancel()
	if err := m.jar.Clear(); err != nil {
		return err
	}
	if err := m.br.ClearSite(ctx, m.spec.Domains, m.spec.Origins); err != nil && !errors.Is(err, browser.ErrNoBrowser) {
		return fmt.Errorf("%s: clear the browser profile: %w", m.spec.Platform, err)
	}
	m.spec.Log.Info("signed out", "platform", m.spec.Platform)
	return nil
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
