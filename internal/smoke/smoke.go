// Package smoke runs the release run's smoke test on the built archive
// (docs/AUTOPILOT.md → Verify gate and smoke tests): the factorio adapter
// loads the mod (and a save) in the owner's Factorio install with isolated
// temp directories; the command adapter runs a script from the publish
// profile. Processes run through injected executors (job objects in the app).
package smoke

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
)

// DefaultTimeout bounds one smoke test (all its processes).
const DefaultTimeout = 10 * time.Minute

// ErrUnavailable: the adapter cannot run here (no install, unsupported
// flags, missing dependency) — not a failure of the mod.
var ErrUnavailable = errors.New("smoke: not available")

// CmdResult is one process run (the release engine's CmdResult shape).
type CmdResult struct {
	Command    string `json:"command"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output,omitempty"` // tail
	DurationMS int64  `json:"durationMs"`
	TimedOut   bool   `json:"timedOut,omitempty"`
}

// ExecFunc runs argv in dir (job object in the app; logDir receives its
// process list) with a timeout.
type ExecFunc func(ctx context.Context, dir string, argv []string, logDir string, timeout time.Duration) CmdResult

// ShellFunc runs a shell command line in dir (the verify shell in the app).
type ShellFunc func(ctx context.Context, dir, command, logDir string, timeout time.Duration) CmdResult

// Request is one smoke test of a built archive.
type Request struct {
	Profile config.SmokeProfile
	Archive string // the built archive (read only: copied into WorkDir)
	Name    string // mod name ({name})
	Version string // released version ({version})
	WorkDir string // fresh temp directory, removed by the caller
	LogDir  string
}

// Result is the smoke test's outcome (stored on the step, output trimmed).
type Result struct {
	Kind       string      `json:"kind"`
	OK         bool        `json:"ok"`
	Install    string      `json:"install,omitempty"` // factorio: the executable used
	Version    string      `json:"version,omitempty"` // factorio: the installed game version
	Steps      []CmdResult `json:"steps"`
	Errors     []string    `json:"errors,omitempty"` // mod error lines from the log
	Note       string      `json:"note,omitempty"`
	DurationMS int64       `json:"durationMs"`
}

// Summary is a short failure text: the failing command, its exit code and the error lines or output tail.
func (r Result) Summary() string {
	var b strings.Builder
	for _, s := range r.Steps {
		if !s.OK {
			fmt.Fprintf(&b, "%s: exit %d", s.Command, s.ExitCode)
			if s.TimedOut {
				b.WriteString(" (timed out)")
			}
			break
		}
	}
	if len(r.Errors) > 0 {
		if b.Len() > 0 {
			b.WriteString(": ")
		}
		b.WriteString(strings.Join(r.Errors, " | "))
	} else {
		for _, s := range r.Steps {
			if !s.OK && s.Output != "" {
				b.WriteString(": " + tail(s.Output, 1500))
				break
			}
		}
	}
	if b.Len() == 0 {
		return r.Note
	}
	return b.String()
}

// Runner runs smoke tests; zero fields get the app defaults.
type Runner struct {
	Exec  ExecFunc
	Shell ShellFunc
	// Detect finds the owner's Factorio install (default DetectFactorio).
	Detect  func() (Install, error)
	Timeout time.Duration
	Now     func() time.Time
}

// Run runs req's adapter. ErrUnavailable (wrapped) = it cannot run here; a
// failed test is a Result with OK false and a nil error.
func (r Runner) Run(ctx context.Context, req Request) (Result, error) {
	if r.Timeout <= 0 {
		r.Timeout = DefaultTimeout
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	start := r.Now()
	if err := os.MkdirAll(req.WorkDir, 0o750); err != nil {
		return Result{Kind: req.Profile.Kind}, err
	}
	var (
		res Result
		err error
	)
	switch req.Profile.Kind {
	case config.SmokeFactorio:
		res, err = r.factorio(ctx, req)
	case config.SmokeCommand:
		res, err = r.command(ctx, req)
	default:
		return Result{Kind: req.Profile.Kind}, fmt.Errorf("%w: no smoke adapter %q", ErrUnavailable, req.Profile.Kind)
	}
	res.Kind = req.Profile.Kind
	if res.Steps == nil {
		res.Steps = []CmdResult{}
	}
	res.DurationMS = r.Now().Sub(start).Milliseconds()
	return res, err
}

// remaining is the time left before ctx's deadline (the per-process timeout).
func remaining(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d)
	}
	return DefaultTimeout
}

// command runs the profile's command in WorkDir with {archive}, {name} and
// {version} replaced (the archive is copied into WorkDir first). Pass = exit 0.
func (r Runner) command(ctx context.Context, req Request) (Result, error) {
	if r.Shell == nil {
		return Result{}, fmt.Errorf("%w: commands cannot run here", ErrUnavailable)
	}
	if strings.TrimSpace(req.Profile.Command) == "" {
		return Result{}, fmt.Errorf("%w: no smoke command in the publish profile", ErrUnavailable)
	}
	archive := filepath.Join(req.WorkDir, filepath.Base(req.Archive))
	if err := copyFile(req.Archive, archive); err != nil {
		return Result{}, err
	}
	line := strings.NewReplacer("{archive}", `"`+archive+`"`, "{name}", req.Name, "{version}", req.Version).Replace(req.Profile.Command)
	c := r.Shell(ctx, req.WorkDir, line, req.LogDir, remaining(ctx))
	c.Output = tail(c.Output, 4000)
	return Result{OK: c.OK, Steps: []CmdResult{c}}, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src) //nolint:gosec // G304: the run's own archive / the owner's configured save
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
