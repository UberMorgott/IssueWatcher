// Package steamcmd publishes Steam Workshop updates with Valve's steamcmd,
// driven by the app: steamcmd +login <user> +workshop_build_item <vdf> +quit
// (VDF: appid, publishedfileid, contentfolder, changenote).
//
// Sign-in: the owner types the login, password and Steam Guard code once in
// Settings › Платформы › Steam › «Вход для загрузки»; the app types them into
// steamcmd's console (a pseudo console: steamcmd reads its prompts from the
// console, not from a redirected stdin), never on a command line or in a log.
// steamcmd caches the sign-in (its config\config.vdf); later runs log in with
// the user name alone. A run that meets a password or Steam Guard prompt
// instead is an expired sign-in: ErrRelogin, the owner signs in again.
package steamcmd

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/conpty"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/secret"
)

// Platform is the provider id the publisher serves.
const Platform = "steam"

// DownloadURL is Valve's official steamcmd for Windows.
const DownloadURL = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"

const settingsFile = "steamcmd.json"

// ErrNoLogin: no steamcmd sign-in yet. ErrRelogin: the cached sign-in is
// gone or refused. ErrNoSteamCMD: steamcmd is not installed.
var (
	ErrNoLogin    = fmt.Errorf("steam: no steamcmd sign-in (Settings › Платформы › Steam › Вход для загрузки): %w", provider.ErrNoUploadAuth)
	ErrRelogin    = fmt.Errorf("steam: the steamcmd sign-in expired, sign in again (Settings › Платформы › Steam › Вход для загрузки): %w", provider.ErrUploadAuthRefused)
	ErrNoSteamCMD = fmt.Errorf("steam: steamcmd not found (Settings › Платформы › Steam › Вход для загрузки › Скачать): %w", provider.ErrNoUploadAuth)
	ErrBusy       = errors.New("steam: steamcmd is busy (a sign-in or an upload is running)")
)

// Proc is a running steamcmd (conpty.Proc).
type Proc = conpty.Proc

// Runner starts exe with args in a pseudo console (tests: a fake steamcmd).
type Runner func(exe string, args []string, dir string) (Proc, error)

// Options configures the Workshop publisher.
type Options struct {
	DataDir  string // the app's data dir: secrets\steamcmd.json, tools\steamcmd
	HTTP     *http.Client
	APIURL   string // https://api.steampowered.com (tests)
	Site     string // https://steamcommunity.com (tests)
	Download string // DownloadURL override (tests)
	Run      Runner // default conpty.Start
	// Candidates are extra places to look for steamcmd.exe (default: the
	// drives' top three folder levels, see locate).
	Candidates func() []string
	Now        func() time.Time
	// OnRelogin is called once when a run finds the cached sign-in expired.
	OnRelogin func()
	// Timeouts: sign-in (default 10 min: the first run updates steamcmd),
	// session check (5 min), upload (60 min).
	LoginTimeout, CheckTimeout, UploadTimeout time.Duration
}

// Workshop is the Steam Workshop publisher (provider.Publisher).
type Workshop struct {
	opts Options
	run  sync.Mutex // one steamcmd at a time
	mu   sync.Mutex // settings + login state
	cur  *loginRun
	last LoginState
}

var _ provider.Publisher = (*Workshop)(nil)

// New returns the publisher.
func New(opts Options) *Workshop {
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	if opts.APIURL == "" {
		opts.APIURL = "https://api.steampowered.com"
	}
	if opts.Site == "" {
		opts.Site = "https://steamcommunity.com"
	}
	if opts.Download == "" {
		opts.Download = DownloadURL
	}
	if opts.Run == nil {
		opts.Run = func(exe string, args []string, dir string) (Proc, error) {
			return conpty.Start(exe, args, conpty.Options{Dir: dir})
		}
	}
	if opts.Candidates == nil {
		opts.Candidates = driveCandidates
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LoginTimeout == 0 {
		opts.LoginTimeout = 10 * time.Minute
	}
	if opts.CheckTimeout == 0 {
		opts.CheckTimeout = 5 * time.Minute
	}
	if opts.UploadTimeout == 0 {
		opts.UploadTimeout = 60 * time.Minute
	}
	return &Workshop{opts: opts}
}

// --- settings ---------------------------------------------------------------

// settings is data\secrets\steamcmd.json (owner-only, DPAPI): no password.
type settings struct {
	User       string    `json:"user,omitempty"`
	Path       string    `json:"path,omitempty"`  // steamcmd.exe
	Found      bool      `json:"found,omitempty"` // Path was found on the drives, not set
	LoggedInAt time.Time `json:"loggedInAt,omitzero"`
	CheckedAt  time.Time `json:"checkedAt,omitzero"`
	Expired    bool      `json:"expired,omitempty"`
}

func (w *Workshop) settingsPath() string {
	return filepath.Join(w.opts.DataDir, "secrets", settingsFile)
}

func (w *Workshop) load() (settings, error) {
	var s settings
	err := secret.ReadProtectedJSON(w.settingsPath(), &s)
	if errors.Is(err, secret.ErrNotFound) {
		return settings{}, nil
	}
	return s, err
}

func (w *Workshop) update(f func(*settings)) (settings, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, err := w.load()
	if err != nil {
		return s, err
	}
	f(&s)
	return s, secret.WriteProtectedJSON(w.settingsPath(), s)
}

// Status is the non-secret view for Settings › Платформы.
type Status struct {
	SteamCMD   string     `json:"steamcmd,omitempty"` // steamcmd.exe in use ("" = none)
	Source     string     `json:"source,omitempty"`   // app | found | path | configured
	User       string     `json:"user,omitempty"`
	LoggedIn   bool       `json:"loggedIn"` // a cached sign-in that last worked
	Expired    bool       `json:"expired,omitempty"`
	LoggedInAt string     `json:"loggedInAt,omitempty"`
	CheckedAt  string     `json:"checkedAt,omitempty"`
	Login      LoginState `json:"login"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Status reports steamcmd and the sign-in (finds steamcmd when none is set).
func (w *Workshop) Status() (Status, error) {
	exe, src, _ := w.steamcmd()
	w.mu.Lock()
	defer w.mu.Unlock()
	s, err := w.load()
	if err != nil {
		return Status{}, err
	}
	st := Status{SteamCMD: exe, Source: src, User: s.User, LoggedIn: s.User != "" && !s.Expired && !s.LoggedInAt.IsZero(),
		Expired: s.Expired, LoggedInAt: stamp(s.LoggedInAt), CheckedAt: stamp(s.CheckedAt), Login: w.last}
	if w.cur != nil {
		st.Login = w.cur.state()
	}
	return st, nil
}

// SetPath sets steamcmd.exe ("" = find it again).
func (w *Workshop) SetPath(path string) (Status, error) {
	path = strings.TrimSpace(path)
	if path != "" {
		if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Base(path), "steamcmd.exe") {
			return Status{}, fmt.Errorf("%w: the full path of steamcmd.exe", provider.ErrBadPublish)
		}
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			return Status{}, fmt.Errorf("%w: %s not found", provider.ErrBadPublish, path)
		}
	}
	if _, err := w.update(func(s *settings) { s.Path, s.Found = path, false }); err != nil {
		return Status{}, err
	}
	return w.Status()
}

// Forget drops the stored user and sign-in state (steamcmd's own cache stays
// until the next sign-in replaces it).
func (w *Workshop) Forget() (Status, error) {
	if _, err := w.update(func(s *settings) { *s = settings{Path: s.Path} }); err != nil {
		return Status{}, err
	}
	return w.Status()
}

// --- locating and installing steamcmd -----------------------------------------

func (w *Workshop) appExe() string {
	return filepath.Join(w.opts.DataDir, "tools", "steamcmd", "steamcmd.exe")
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// steamcmd is the steamcmd.exe to use: the configured one, the app's own
// download, one on PATH, else one found on the drives (remembered).
func (w *Workshop) steamcmd() (string, string, error) {
	w.mu.Lock()
	s, err := w.load()
	w.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	if s.Path != "" && isFile(s.Path) {
		if strings.EqualFold(filepath.Clean(s.Path), filepath.Clean(w.appExe())) {
			return s.Path, "app", nil
		}
		if s.Found {
			return s.Path, "found", nil
		}
		return s.Path, "configured", nil
	}
	if p := w.appExe(); isFile(p) {
		return p, "app", nil
	}
	if p, err := exec.LookPath("steamcmd.exe"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, "path", nil
		}
	}
	for _, p := range w.opts.Candidates() {
		if isFile(p) {
			_, _ = w.update(func(s *settings) { s.Path, s.Found = p, true })
			return p, "found", nil
		}
	}
	return "", "", ErrNoSteamCMD
}

// candidatePatterns are where people unpack steamcmd: <drive>\steamcmd,
// <drive>\<dir>\steamcmd, <drive>\<dir>\<dir>\steamcmd.
var candidatePatterns = []string{`steamcmd\steamcmd.exe`, `*\steamcmd\steamcmd.exe`, `*\*\steamcmd\steamcmd.exe`}

// driveCandidates globs candidatePatterns on every drive letter that exists.
func driveCandidates() []string {
	var out []string
	for d := 'C'; d <= 'Z'; d++ {
		root := string(d) + `:\`
		if _, err := os.Stat(root); err != nil {
			continue
		}
		for _, pat := range candidatePatterns {
			m, _ := filepath.Glob(root + pat)
			out = append(out, m...)
		}
	}
	return out
}

// Install downloads Valve's steamcmd.zip into data\tools\steamcmd (the
// owner's click) and uses it. steamcmd updates itself on its first run.
func (w *Workshop) Install(ctx context.Context) (Status, error) {
	if !w.run.TryLock() {
		return Status{}, ErrBusy
	}
	defer w.run.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.opts.Download, nil)
	if err != nil {
		return Status{}, err
	}
	res, err := w.opts.HTTP.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("steam: download steamcmd: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("steam: download steamcmd: HTTP %d", res.StatusCode)
	}
	const maxZip = 32 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, maxZip+1))
	if err != nil {
		return Status{}, fmt.Errorf("steam: download steamcmd: %w", err)
	}
	if len(b) > maxZip {
		return Status{}, errors.New("steam: download steamcmd: unexpectedly large")
	}
	zr, err := zip.NewReader(strings.NewReader(string(b)), int64(len(b)))
	if err != nil {
		return Status{}, fmt.Errorf("steam: steamcmd.zip: %w", err)
	}
	var exe *zip.File
	for _, f := range zr.File {
		if f.Name == "steamcmd.exe" {
			exe = f
		}
	}
	if exe == nil {
		return Status{}, errors.New("steam: steamcmd.zip has no steamcmd.exe")
	}
	dst := w.appExe()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return Status{}, err
	}
	rc, err := exe.Open()
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = rc.Close() }()
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700) //nolint:gosec // G302: an executable
	if err != nil {
		return Status{}, err
	}
	if _, err := io.Copy(f, io.LimitReader(rc, maxZip)); err != nil {
		_ = f.Close()
		return Status{}, err
	}
	if err := f.Close(); err != nil {
		return Status{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return Status{}, err
	}
	if _, err := w.update(func(s *settings) { s.Path, s.Found = dst, false }); err != nil {
		return Status{}, err
	}
	return w.Status()
}

// --- console output -----------------------------------------------------------

// console collects a steamcmd's output (VT sequences removed).
type console struct {
	mu   sync.Mutex
	text strings.Builder
	done chan struct{}
	ping chan struct{}
}

func readConsole(p Proc) *console {
	c := &console{done: make(chan struct{}), ping: make(chan struct{}, 1)}
	go func() {
		defer close(c.done)
		buf := make([]byte, 4096)
		for {
			n, err := p.Output().Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.text.Write(buf[:n])
				c.mu.Unlock()
				select {
				case c.ping <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}

// String is the output so far without VT sequences (stripped as a whole: a
// sequence may span reads).
func (c *console) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return conpty.Plain(c.text.String())
}

// lock takes the one-steamcmd lock, waiting up to wait (a sign-in finishing).
func (w *Workshop) lock(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for !w.run.TryLock() {
		if time.Now().After(deadline) {
			return ErrBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// Output patterns (steamcmd prints these in English whatever the system
// language; its updater lines are localized and not matched).
var (
	passwordRe = regexp.MustCompile(`(?i)password:\s*$`)
	guardRe    = regexp.MustCompile(`(?i)(steam guard code|two-factor code|two factor code|auth code)[^:\n]*:\s*$`)
	mobileRe   = regexp.MustCompile(`(?i)steam mobile app|confirm the login|waiting for confirmation`)
	loggedInRe = regexp.MustCompile(`(?i)waiting for user info\.*\s*ok|logged in ok`)
	cachedNone = regexp.MustCompile(`(?i)cached credentials not found|no cached credentials`)
	failRe     = regexp.MustCompile(`(?im)^.*\b(?:failed|error)\b[^\n]*\([^)\n]*\)[^\n]*$|^.*login failure[^\n]*$`)
	successRe  = regexp.MustCompile(`(?m)^\s*Success\.`)
	uploadErr  = regexp.MustCompile(`(?im)^.*ERROR! Failed to update workshop item[^\n]*$`)
)

// scrub removes secrets from a line shown to the owner.
func scrub(s string, secrets ...string) string {
	for _, x := range secrets {
		if len(x) >= 3 {
			s = strings.ReplaceAll(s, x, "***")
		}
	}
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// lastLine is the last match of re in s ("" = none).
func lastLine(re *regexp.Regexp, s string) string {
	m := re.FindAllString(s, -1)
	if len(m) == 0 {
		return ""
	}
	return strings.TrimSpace(m[len(m)-1])
}
