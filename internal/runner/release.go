package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Hooks for the autopilot release run (internal/release): it shares the
// mapped-folder lock with fix jobs, runs the project's verify gate and the
// publish profile's build command in job objects, and pushes with the same
// token environment.

// ErrFolderBusy: a fix job (or another release step) is using the folder.
var ErrFolderBusy = errors.New("runner: the project folder is in use by a running job")

// AcquireFolder reserves the mapped folder path for an actor outside the
// queue (one actor per checkout): queued jobs of that folder wait until
// release is called. A job running there → ErrFolderBusy.
func (r *Runner) AcquireFolder(path string) (release func(), err error) {
	key := folderKey(path)
	if key == "" {
		return nil, errors.New("runner: no folder")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.held[key] {
		return nil, ErrFolderBusy
	}
	for _, a := range r.running {
		if a.folder == key {
			return nil, ErrFolderBusy
		}
	}
	if r.held == nil {
		r.held = map[string]bool{}
	}
	r.held[key] = true
	done := false
	return func() {
		r.mu.Lock()
		if !done {
			done = true
			delete(r.held, key)
		}
		r.mu.Unlock()
		r.kick()
	}, nil
}

// Gate runs the static verify gate of project (platform:external_id) in dir:
// `aegis verify` when localPath has Aegis enabled, else the project's verify
// command. ran is false when the project has neither. logDir receives the
// process list while it runs (crash cleanup).
func (r *Runner) Gate(ctx context.Context, project, localPath, dir, logDir string) (res VerifyResult, ran bool) {
	pa := r.opts.Settings().Agents.Projects[project]
	argv, label := r.verifyCommand(localPath, dir, pa)
	if argv == nil {
		return VerifyResult{}, false
	}
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return VerifyResult{Command: label, ExitCode: -1, Output: err.Error()}, true
	}
	return r.verify(ctx, dir, logDir, argv, label, &jobLog{}), true
}

// RunCommand runs command through the verify shell in dir inside a job
// object (the whole tree is killed on timeout), like the verify step.
func (r *Runner) RunCommand(ctx context.Context, dir, command, logDir string, timeout time.Duration) VerifyResult {
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return VerifyResult{Command: command, ExitCode: -1, Output: err.Error()}
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	argv := append(append([]string{}, r.opts.Shell...), command)
	return r.verify(ctx, dir, filepath.Clean(logDir), argv, command, &jobLog{})
}

// TokenEnv is tokenEnv for other packages: the token reaches git through
// GIT_CONFIG_* scoped to url, credential helpers off; secret is the value to
// redact from errors.
func TokenEnv(url, token string) (env []string, secret string) { return tokenEnv(url, token) }
