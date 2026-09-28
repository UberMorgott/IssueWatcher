package selfupdate

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"
)

// Updater states.
const (
	StateIdle        = "idle"
	StateChecking    = "checking"
	StateDownloading = "downloading"
	StateInstalling  = "installing" // swap + waiting for the new process
	StateRestarting  = "restarting" // new process ready; this one shuts down
)

// Prefs are the user's update settings (config.Updates).
type Prefs struct {
	Channel   string
	AutoCheck bool
	Interval  time.Duration
}

// Options configures an Updater.
type Options struct {
	Current string // running version (main.Version)
	Exe     string // running executable
	DataDir string
	PID     int
	Source  Source
	Prefs   func() Prefs
	Log     *slog.Logger
	// OnChange gets every status change (SSE update.status).
	OnChange func(Status)
	// Quit shuts the app down gracefully once the new process is ready.
	Quit func()
	// LastResult is the outcome of the previous update (TakeResult or the
	// after-update start).
	LastResult *Result
	// ReadyTimeout bounds the wait for the new process (0 = 30 s).
	ReadyTimeout time.Duration
	// FirstCheck delays the first automatic check after start (0 = 1 min;
	// jitter of up to a minute is added).
	FirstCheck time.Duration
	// PublicKey verifies manifests; nil = the key compiled into this build.
	PublicKey ed25519.PublicKey
}

// Available is the newest release of the channel.
type Available struct {
	Version     string    `json:"version"`
	Name        string    `json:"name,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	PublishedAt time.Time `json:"publishedAt"`
	URL         string    `json:"url,omitempty"`
	Prerelease  bool      `json:"prerelease"`
}

// Status is GET /api/update and the update.status live event.
type Status struct {
	Current         string     `json:"current"`
	Channel         string     `json:"channel"`
	DevBuild        bool       `json:"devBuild"` // not a release: checks only, never installs
	State           string     `json:"state"`
	Error           string     `json:"error,omitempty"`
	Done            int64      `json:"done,omitempty"` // download progress, bytes
	Total           int64      `json:"total,omitempty"`
	Available       *Available `json:"available,omitempty"`
	UpdateAvailable bool       `json:"updateAvailable"`
	CheckedAt       *time.Time `json:"checkedAt,omitempty"`
	LastResult      *Result    `json:"lastResult,omitempty"`
}

// Errors of Install.
var (
	ErrBusy     = errors.New("an update check or install is already running")
	ErrNoUpdate = errors.New("no newer version to install: check first")
	ErrDevBuild = errors.New("this is a development build: updates install only over a release")
)

// Updater checks for and installs releases.
type Updater struct {
	opts Options

	mu        sync.Mutex
	state     string
	err       string
	done      int64
	total     int64
	rel       *Release
	checkedAt time.Time
	lastPub   time.Time
}

// New returns an idle Updater.
func New(opts Options) *Updater {
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 30 * time.Second
	}
	if opts.FirstCheck <= 0 {
		opts.FirstCheck = time.Minute
	}
	return &Updater{opts: opts, state: StateIdle}
}

// Status is the current state.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.statusLocked()
}

func (u *Updater) statusLocked() Status {
	st := Status{
		Current: u.opts.Current, Channel: u.opts.Prefs().Channel, DevBuild: !IsRelease(u.opts.Current),
		State: u.state, Error: u.err, Done: u.done, Total: u.total, LastResult: u.opts.LastResult,
	}
	if r := u.rel; r != nil {
		st.Available = &Available{Version: r.Tag, Name: r.Name, Notes: r.Notes, PublishedAt: r.PublishedAt, URL: r.HTMLURL, Prerelease: r.Prerelease}
		st.UpdateAvailable = Newer(r.Tag, u.opts.Current)
	}
	if !u.checkedAt.IsZero() {
		t := u.checkedAt
		st.CheckedAt = &t
	}
	return st
}

// set changes the state under u.mu and publishes the new status.
func (u *Updater) set(f func()) {
	u.mu.Lock()
	f()
	st := u.statusLocked()
	u.lastPub = time.Now()
	u.mu.Unlock()
	if u.opts.OnChange != nil {
		u.opts.OnChange(st)
	}
}

// Check asks GitHub for the newest release of the configured channel.
func (u *Updater) Check(ctx context.Context) (Status, error) {
	u.mu.Lock()
	if u.state != StateIdle {
		st := u.statusLocked()
		u.mu.Unlock()
		return st, ErrBusy
	}
	u.mu.Unlock()
	u.set(func() { u.state, u.err = StateChecking, "" })
	channel := u.opts.Prefs().Channel
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rel, err := u.opts.Source.Latest(ctx, channel)
	u.set(func() {
		u.state = StateIdle
		if err != nil {
			u.err = err.Error()
			return
		}
		u.rel, u.checkedAt = rel, time.Now().UTC()
	})
	if err != nil {
		u.opts.Log.Warn("update: check failed", "channel", channel, "err", err)
		return u.Status(), err
	}
	st := u.Status()
	u.opts.Log.Info("update: checked", "channel", channel, "current", u.opts.Current,
		"latest", func() string {
			if rel == nil {
				return "none"
			}
			return rel.Tag
		}(), "update", st.UpdateAvailable)
	return st, nil
}

// Install starts downloading and installing the checked release in the
// background; progress and the outcome arrive through OnChange.
func (u *Updater) Install() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.state != StateIdle:
		return ErrBusy
	case !IsRelease(u.opts.Current):
		return ErrDevBuild
	case u.rel == nil || !Newer(u.rel.Tag, u.opts.Current):
		return ErrNoUpdate
	}
	rel := u.rel
	u.state, u.err, u.done, u.total = StateDownloading, "", 0, 0
	st := u.statusLocked()
	go func() {
		if u.opts.OnChange != nil {
			u.opts.OnChange(st)
		}
		u.install(rel)
	}()
	return nil
}

func (u *Updater) fail(err error) {
	u.opts.Log.Error("update: failed", "err", err)
	u.set(func() { u.state, u.err, u.done, u.total = StateIdle, err.Error(), 0, 0 })
}

func (u *Updater) install(rel *Release) {
	pub := u.opts.PublicKey
	if pub == nil {
		var err error
		if pub, err = PublicKey(); err != nil {
			u.fail(err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	u.opts.Log.Info("update: downloading", "version", rel.Tag)
	m, err := u.opts.Source.Prepare(ctx, rel, pub, u.opts.Current, u.opts.Exe, u.progress)
	if err != nil {
		u.fail(err)
		return
	}
	u.opts.Log.Info("update: verified", "version", m.Version, "size", m.Size, "sha256", m.SHA256)
	u.set(func() { u.state = StateInstalling })
	if err := Swap(u.opts.Exe); err != nil {
		_ = Cleanup(ctx, u.opts.Exe, 1, 0)
		u.fail(err)
		return
	}
	ClearReady(u.opts.DataDir)
	p, err := Start(u.opts.Exe, "--after-update="+strconv.Itoa(u.opts.PID))
	if err != nil {
		u.rollback(fmt.Errorf("start the new version: %w", err))
		return
	}
	exited := make(chan struct{})
	go func() { _, _ = p.Wait(); close(exited) }()
	rctx, rcancel := context.WithTimeout(ctx, u.opts.ReadyTimeout)
	err = waitReady(rctx, u.opts.DataDir, p.Pid, exited)
	rcancel()
	if err != nil {
		_ = p.Kill()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
		u.rollback(err)
		return
	}
	ClearReady(u.opts.DataDir)
	u.opts.Log.Info("update: new version ready; shutting down", "version", m.Version, "new_pid", p.Pid)
	u.set(func() { u.state = StateRestarting })
	time.Sleep(300 * time.Millisecond) // let update.status reach the open tabs
	u.opts.Quit()
}

// rollback puts the old executable back after the new one failed to start.
func (u *Updater) rollback(cause error) {
	if err := Restore(u.opts.Exe); err != nil {
		u.fail(fmt.Errorf("%w; rollback failed: %w", cause, err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Cleanup(ctx, u.opts.Exe, 40, 250*time.Millisecond); err != nil {
		u.opts.Log.Warn("update: cleanup after rollback", "err", err)
	}
	u.opts.Log.Warn("update: rolled back", "cause", cause)
	u.fail(fmt.Errorf("%w (rolled back, %s still running)", cause, u.opts.Current))
}

// progress publishes download progress at most every 200 ms.
func (u *Updater) progress(done, total int64) {
	u.mu.Lock()
	u.done, u.total = done, total
	due := time.Since(u.lastPub) >= 200*time.Millisecond || done == total
	u.mu.Unlock()
	if due {
		u.set(func() {})
	}
}

// Run checks automatically (Prefs.AutoCheck) until ctx ends: first after
// FirstCheck, then every Prefs.Interval, each wait with ±10 % jitter. It never
// installs on its own.
func (u *Updater) Run(ctx context.Context) {
	wait := u.opts.FirstCheck + rand.N(time.Minute) //nolint:gosec // G404: jitter, not security
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		p := u.opts.Prefs()
		if p.AutoCheck {
			_, _ = u.Check(ctx)
		}
		iv := max(p.Interval, time.Hour)
		wait = iv - iv/10 + rand.N(iv/5) //nolint:gosec // G404: jitter, not security
	}
}
