// Package steamcmd is the Steam Workshop publisher (platform steam). Uploads
// go through the owner's running, signed-in Steam client (Options.Upload,
// steamugc's ISteamUGC helper): steamcmd's own "+login" replaced the Steam
// client's session ("Session Replaced") and signed the owner out, so
// steamcmd is no longer used for uploads.
//
// The steamcmd driver below (provisioning, sign-in) is kept for now but no
// upload path uses it. Sign-in: the owner types the login, password and Steam Guard code once in
// Settings › Платформы › Steam › «Вход для загрузки»; the app types them into
// steamcmd's console (a pseudo console: steamcmd reads its prompts from the
// console, not from a redirected stdin), never on a command line or in a log.
// steamcmd caches the sign-in (its config\config.vdf); later runs log in with
// the user name alone. A run that meets a password or Steam Guard prompt
// instead is an expired sign-in: ErrRelogin, the owner signs in again.
package steamcmd

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/conpty"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/steamugc"
	"github.com/UberMorgott/issuewatcher/internal/secret"
	"github.com/UberMorgott/issuewatcher/internal/tools"
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
	ErrNoSteamCMD = fmt.Errorf("steam: steamcmd is not set up yet (Settings › Платформы › Steam › Инструменты): %w", provider.ErrNoUploadAuth)
	ErrBusy       = errors.New("steam: steamcmd is busy (a sign-in or an upload is running)")
)

// Proc is a running steamcmd (conpty.Proc).
type Proc = conpty.Proc

// Runner starts exe with args in a pseudo console (tests: a fake steamcmd).
type Runner func(exe string, args []string, dir string) (Proc, error)

// Uploader sends a content folder as a Workshop item's new content through
// the running Steam client (steamugc.Client).
type Uploader interface {
	Upload(ctx context.Context, appID uint32, item uint64, dir, note string, progress func(steamugc.UploadProgress)) (steamugc.UploadResult, error)
	// Ready: the Steam API DLL is set up or can be (no writes).
	Ready() error
}

// Options configures the Workshop publisher.
type Options struct {
	// Upload publishes content (required for Publish).
	Upload   Uploader
	DataDir  string // the app's data dir: secrets\steamcmd.json, tools\steamcmd
	HTTP     *http.Client
	APIURL   string // https://api.steampowered.com (tests)
	Site     string // https://steamcommunity.com (tests)
	Download string // DownloadURL override (tests)
	Run      Runner // default conpty.Start
	Log      func(msg string, err error)
	Now      func() time.Time
	// OnRelogin is called once when a run finds the cached sign-in expired.
	OnRelogin func()
	// Timeouts: sign-in (default 10 min: the first run updates steamcmd),
	// session check (5 min), upload (60 min).
	LoginTimeout, CheckTimeout, UploadTimeout time.Duration
	// LogPoll: how often a sign-in reads steamcmd's connection log (1 s).
	LogPoll time.Duration
	// WarmTimeout bounds the first "+quit" run after the download, which
	// lets steamcmd update itself before any sign-in (10 min).
	WarmTimeout time.Duration
}

// Workshop is the Steam Workshop publisher (provider.Publisher).
type Workshop struct {
	opts Options
	run  sync.Mutex // one steamcmd at a time
	mu   sync.Mutex // settings + login state
	cur  *loginRun
	last LoginState
	prog tools.Progress // steamcmd's provisioning
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
	if opts.Log == nil {
		opts.Log = func(string, error) {}
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
	if opts.LogPoll == 0 {
		opts.LogPoll = time.Second
	}
	if opts.WarmTimeout == 0 {
		opts.WarmTimeout = 10 * time.Minute
	}
	return &Workshop{opts: opts}
}

// --- settings ---------------------------------------------------------------

// settings is data\secrets\steamcmd.json (owner-only, DPAPI): no password.
type settings struct {
	User string `json:"user,omitempty"`
	// Path / Found: a steamcmd outside the app folder used by older versions;
	// only read once to carry its sign-in cache over (migrate), then dropped.
	Path       string    `json:"path,omitempty"`
	Found      bool      `json:"found,omitempty"`
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
	SteamCMD   string       `json:"steamcmd,omitempty"` // the app's steamcmd.exe ("" = not set up yet)
	Tool       tools.Status `json:"tool"`
	User       string       `json:"user,omitempty"`
	LoggedIn   bool         `json:"loggedIn"` // a cached sign-in that last worked
	Expired    bool         `json:"expired,omitempty"`
	LoggedInAt string       `json:"loggedInAt,omitempty"`
	CheckedAt  string       `json:"checkedAt,omitempty"`
	Login      LoginState   `json:"login"`
}

func stamp(t time.Time) string { return tools.Stamp(t) }

// Status reports steamcmd and the sign-in.
func (w *Workshop) Status() (Status, error) {
	exe, _ := w.steamcmd()
	tool := w.Tool()
	w.mu.Lock()
	defer w.mu.Unlock()
	s, err := w.load()
	if err != nil {
		return Status{}, err
	}
	st := Status{SteamCMD: exe, Tool: tool, User: s.User, LoggedIn: s.User != "" && !s.Expired && !s.LoggedInAt.IsZero(),
		Expired: s.Expired, LoggedInAt: stamp(s.LoggedInAt), CheckedAt: stamp(s.CheckedAt), Login: w.last}
	if w.cur != nil {
		st.Login = w.cur.state()
	}
	return st, nil
}

// Forget drops the stored user and sign-in state (steamcmd's own cache stays
// until the next sign-in replaces it).
func (w *Workshop) Forget() (Status, error) {
	if _, err := w.update(func(s *settings) { *s = settings{Path: s.Path} }); err != nil {
		return Status{}, err
	}
	return w.Status()
}

// --- provisioning steamcmd ----------------------------------------------------

// steamcmd lives only in data\tools\steamcmd (portable: no PATH lookup, no
// other program's folder); its sign-in cache (config\config.vdf) and its
// self-update stay there too.
func (w *Workshop) appExe() string {
	return filepath.Join(tools.Dir(w.opts.DataDir, "steamcmd"), "steamcmd.exe")
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// steamcmd is the app's own steamcmd.exe (ErrNoSteamCMD until provisioned).
func (w *Workshop) steamcmd() (string, error) {
	if p := w.appExe(); isFile(p) {
		return p, nil
	}
	return "", ErrNoSteamCMD
}

// Tool is steamcmd's provisioning status.
func (w *Workshop) Tool() tools.Status {
	st := tools.Status{Name: "steamcmd", Source: w.opts.Download}
	ready := isFile(w.appExe())
	if ready {
		st.Path = w.appExe()
		if fi, err := os.Stat(st.Path); err == nil {
			st.At = tools.Stamp(fi.ModTime())
		}
	}
	w.prog.Apply(&st, ready)
	return st
}

// ensure provisions steamcmd when it is missing (a sign-in or an upload
// before the startup provisioning finished).
func (w *Workshop) ensure(ctx context.Context) (string, error) {
	if p, err := w.steamcmd(); err == nil {
		return p, nil
	}
	if _, err := w.Install(ctx); err != nil {
		return "", err
	}
	return w.steamcmd()
}

// Provision sets steamcmd up once (startup, in the background; the
// Settings button): downloads it when missing and carries an older setup's
// sign-in over. Errors also show in Tool.
func (w *Workshop) Provision(ctx context.Context) (tools.Status, error) {
	_, err := w.Install(ctx)
	if err != nil && !errors.Is(err, tools.ErrBusy) {
		w.opts.Log("steamcmd provisioning failed", err)
	}
	return w.Tool(), err
}

// Install downloads Valve's steamcmd.zip into data\tools\steamcmd when it is
// not there yet (automatic at startup; Settings › Платформы retries it) and
// takes over the sign-in cache of a steamcmd the app used before. steamcmd
// updates itself in that folder on its first run.
func (w *Workshop) Install(ctx context.Context) (Status, error) {
	if !w.prog.Begin() {
		return Status{}, tools.ErrBusy
	}
	err := w.install(ctx)
	w.prog.End(err)
	if err != nil {
		return Status{}, err
	}
	return w.Status()
}

func (w *Workshop) install(ctx context.Context) error {
	if !isFile(w.appExe()) {
		if !w.run.TryLock() {
			return tools.ErrBusy
		}
		err := w.download(ctx)
		if err == nil {
			w.warm(ctx)
		}
		w.run.Unlock()
		if err != nil {
			return err
		}
	}
	return w.migrate()
}

// warm runs the fresh steamcmd once with "+quit": its first run downloads
// and installs its update (~45 MB) and restarts, which otherwise happens in
// the middle of the owner's first sign-in. A failure only logs: the
// sign-in updates it then.
func (w *Workshop) warm(ctx context.Context) {
	b, err := w.runBatch(ctx, w.appExe(), []string{"+quit"}, w.opts.WarmTimeout)
	if err == nil && b.code != 0 {
		err = fmt.Errorf("exit code %d: %s", b.code, lastOutput(b.out))
	}
	if err != nil {
		w.opts.Log("steamcmd: first run (self-update) failed", err)
	}
}

func (w *Workshop) download(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.opts.Download, nil)
	if err != nil {
		return err
	}
	res, err := w.opts.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("steam: download steamcmd: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("steam: download steamcmd: HTTP %d", res.StatusCode)
	}
	const maxZip = 32 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, maxZip+1))
	if err != nil {
		return fmt.Errorf("steam: download steamcmd: %w", err)
	}
	if len(b) > maxZip {
		return errors.New("steam: download steamcmd: unexpectedly large")
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return fmt.Errorf("steam: steamcmd.zip: %w", err)
	}
	var exe *zip.File
	for _, f := range zr.File {
		if f.Name == "steamcmd.exe" {
			exe = f
		}
	}
	if exe == nil {
		return errors.New("steam: steamcmd.zip has no steamcmd.exe")
	}
	dst := w.appExe()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	rc, err := exe.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700) //nolint:gosec // G302: an executable
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(rc, maxZip)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// migrate: an older version ran a steamcmd from another folder (settings
// Path). Its sign-in cache (config\*.vdf) is copied into the app's steamcmd
// once, when the app's has none, and the old path is dropped for good. A
// cache that does not carry over is an expired sign-in: the owner signs in
// once more in the UI.
func (w *Workshop) migrate() error {
	s, err := w.update(func(*settings) {})
	if err != nil || s.Path == "" {
		return err
	}
	old := filepath.Dir(s.Path)
	if !strings.EqualFold(filepath.Clean(old), filepath.Clean(filepath.Dir(w.appExe()))) {
		dst := filepath.Join(filepath.Dir(w.appExe()), "config")
		if !isFile(filepath.Join(dst, "config.vdf")) {
			ms, _ := filepath.Glob(filepath.Join(old, "config", "*.vdf"))
			for _, m := range ms {
				if _, err := tools.CopyFile(m, filepath.Join(dst, filepath.Base(m)), 16<<20); err != nil {
					w.opts.Log("steamcmd: sign-in cache not carried over", err)
				}
			}
		}
	}
	_, err = w.update(func(s *settings) { s.Path, s.Found = "", false })
	return err
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
	// The console stays silent during the mobile approval; the connection
	// log says so (real steamcmd 1788292693, testdata/real_mobile_login).
	confirmLogRe = regexp.MustCompile(`(?m)\] Waiting for confirmation\s*$`)
	updateRe     = regexp.MustCompile(`(?m)^\[\s*(?:\d+%|----)\]`)
	loadingRe    = regexp.MustCompile(`Loading Steam API`)
	loggingInRe  = regexp.MustCompile(`Logging in user '`)
	loggedInRe   = regexp.MustCompile(`(?i)waiting for user info\.*\s*ok|logged in ok`)
	cachedNone   = regexp.MustCompile(`(?i)cached credentials not found|no cached credentials`)
	failRe       = regexp.MustCompile(`(?im)^.*\b(?:failed|error)\b[^\n]*\([^)\n]*\)[^\n]*$|^.*login failure[^\n]*$`)
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
