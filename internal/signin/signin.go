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
//
// One attempt at a time: a «Подключить» registers its attempt (cancel + done)
// before the first probe, so Cancel / «Выйти» stops the whole lifecycle
// (stored probe, profile capture, window) and waits for it; a second
// «Подключить» joins the running one. Each attempt carries the epoch it began
// in: Cancel bumps it, and a session is only stored while it is unchanged.
type Manager struct {
	spec  Spec
	jar   *websession.Jar
	br    Browser
	lease *lease
	gate  chan struct{} // one attempt's checks at a time

	mu      sync.Mutex
	epoch   uint64 // bumped by Cancel: an older attempt stores nothing
	waiting bool
	cancel  context.CancelFunc
	detail  string // why the last window sign-in ended without a session
	done    chan struct{}
}

// lease is one browser's sign-in window. The native engines share one
// browser profile, which runs one window at a time: the platform whose window
// sign-in holds it keeps it until that sign-in ends, and another platform's
// «Подключить» neither stops nor replaces it.
type lease struct {
	mu    sync.Mutex
	owner *Manager
}

var leases sync.Map // Browser → *lease

func leaseOf(br Browser) *lease {
	l, _ := leases.LoadOrStore(br, &lease{})
	return l.(*lease)
}

// take makes m the window's owner; else it returns the current one.
func (l *lease) take(m *Manager) (*Manager, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner != nil && l.owner != m {
		return l.owner, false
	}
	l.owner = m
	return nil, true
}

func (l *lease) release(m *Manager) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner == m {
		l.owner = nil
	}
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
	return &Manager{spec: spec, jar: jar, br: br, lease: leaseOf(br), gate: make(chan struct{}, 1)}
}

// Jar is the stored session.
func (m *Manager) Jar() *websession.Jar { return m.jar }

const detailCancelled = "вход отменён"

// Login implements the three-step «Подключить»; a window sign-in returns
// at once with InProgress (Status reports its end).
func (m *Manager) Login(ctx context.Context) (provider.Login, error) {
	m.mu.Lock()
	epoch := m.epoch
	m.mu.Unlock()
	select { // a parallel «Подключить» waits for the running checks, then joins
	case m.gate <- struct{}{}:
	case <-ctx.Done():
		return provider.Login{}, ctx.Err()
	}
	defer func() { <-m.gate }()
	m.mu.Lock()
	switch {
	case m.waiting:
		m.mu.Unlock()
		return m.windowState(), nil
	case m.epoch != epoch: // cancelled («Выйти») while it waited
		m.mu.Unlock()
		return provider.Login{Detail: detailCancelled}, nil
	}
	m.detail = ""
	actx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.cancel, m.done = cancel, done
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		if m.done == done { // not handed over to a window sign-in
			m.cancel, m.done = nil, nil
		}
		m.mu.Unlock()
		close(done)
	}()
	return m.login(actx, epoch)
}

func (m *Manager) login(ctx context.Context, epoch uint64) (provider.Login, error) {
	// (1) the stored session.
	if !m.jar.Empty() {
		acc, err := m.spec.Probe(ctx, m.jar)
		switch {
		case err == nil:
			if ok, _ := m.store(epoch, func() error { return m.jar.SetAccount(acc) }); !ok {
				return provider.Login{Detail: detailCancelled}, nil
			}
			return provider.Login{LoggedIn: true, Account: acc, Source: cmp(m.jar.Source(), SourceStored)}, nil
		case !errors.Is(err, provider.ErrNotSignedIn):
			return provider.Login{}, err
		}
	}
	// (2) our profile is already signed in.
	if acc, ok, err := m.capture(ctx, SourceProfile, epoch); err != nil {
		return provider.Login{}, err
	} else if ok {
		return provider.Login{LoggedIn: true, Account: acc, Source: SourceProfile, Browser: m.br.Name()}, nil
	}
	if !m.current(epoch) {
		return provider.Login{Detail: detailCancelled}, nil
	}
	// (3) the visible sign-in window, unless another platform's is open.
	if owner, ok := m.lease.take(m); !ok {
		return provider.Login{Detail: fmt.Sprintf("Окно входа уже открыто для %s: завершите или отмените тот вход", owner.spec.Platform)}, nil
	}
	m.br.Stop() // one profile: headless ends before the window opens
	if err := m.br.OpenWindow(ctx, m.spec.LoginURL); err != nil {
		m.lease.release(m)
		return provider.Login{}, fmt.Errorf("%s: open the sign-in window: %w", m.spec.Platform, err)
	}
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), m.spec.Timeout)
	wdone := make(chan struct{})
	m.mu.Lock()
	if m.epoch != epoch { // cancelled while the window opened
		m.mu.Unlock()
		wcancel()
		m.br.Stop()
		m.lease.release(m)
		return provider.Login{Detail: detailCancelled}, nil
	}
	m.waiting, m.cancel, m.done = true, wcancel, wdone
	m.mu.Unlock()
	m.spec.Log.Info("sign-in window opened", "platform", m.spec.Platform, "browser", m.br.Name())
	go m.wait(wctx, wcancel, wdone, epoch)
	l := m.windowState()
	l.Window = true
	return l, nil
}

// current reports whether the attempt of epoch is still wanted.
func (m *Manager) current(epoch uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.epoch == epoch
}

// store runs f (a jar write) unless the attempt of epoch was cancelled; the
// check and the write are atomic with Cancel's epoch bump.
func (m *Manager) store(epoch uint64, f func() error) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.epoch != epoch {
		return false, nil
	}
	return true, f()
}

func (m *Manager) windowState() provider.Login {
	return provider.Login{InProgress: true, Via: "window", ViaBrowser: m.br.Name(), Detail: "Войдите в открывшемся окне браузера"}
}

// capture probes the profile's cookies and stores them when signed in (and
// the attempt of epoch is still current).
func (m *Manager) capture(ctx context.Context, source string, epoch uint64) (string, bool, error) {
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
	ok, err := m.store(epoch, func() error { return m.jar.Replace(cand.Cookies(), cand.UserAgent(), acc, source) })
	if err != nil || !ok {
		return "", false, err
	}
	m.spec.Log.Info("session captured", "platform", m.spec.Platform, "source", source, "account", acc)
	return acc, true, nil
}

func (m *Manager) wait(ctx context.Context, cancel context.CancelFunc, done chan struct{}, epoch uint64) {
	defer close(done)
	defer cancel()
	detail := ""
	defer func() {
		m.br.Stop() // the window closes; reads start headless again
		m.lease.release(m)
		m.mu.Lock()
		m.waiting, m.cancel, m.done, m.detail = false, nil, nil, detail
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
				detail = detailCancelled
			}
			return
		case <-t.C:
		}
		if !m.br.WindowOpen(ctx) {
			detail = "окно входа закрыто"
			return
		}
		pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
		_, ok, err := m.capture(pctx, SourceWindow, epoch)
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

// Cancel stops the running attempt (its checks or its waiting window, which
// closes) and waits for it to end; nothing it still probes is stored.
func (m *Manager) Cancel() {
	m.mu.Lock()
	m.epoch++
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
