package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/instance"
	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
)

// envUpdateBase overrides the GitHub API root the updater asks (tests only: a
// local fake of the releases API). Manifests must still carry the release key's
// signature.
const envUpdateBase = "IW_UPDATE_BASE"

// Timeouts of the update hand-over.
const (
	oldExitWait   = 60 * time.Second // new process: the old one closes sync, HTTP, tray, SQLite
	lockWait      = 5 * time.Second  // the single-instance mutex of a just-exited process
	shutdownLimit = 20 * time.Second // old process: hard exit if a graceful shutdown hangs
)

// launch is how this process was started.
type launch struct {
	afterUpdate int    // --after-update=<pid>: the old version's PID; take over its port
	rolledBack  int    // --rolled-back=<pid>: a failed new version's PID; it put this exe back
	from        string // after update: the version that ran before (its runtime.json)
}

func parseLaunch(args []string) launch {
	var l launch
	for _, a := range args {
		k, v, ok := strings.Cut(strings.TrimLeft(a, "-/"), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			continue
		}
		switch strings.ToLower(k) {
		case "after-update":
			l.afterUpdate = n
		case "rolled-back":
			l.rolledBack = n
		}
	}
	return l
}

// quiet reports a launch that must never open the browser on its own.
func (l launch) quiet() bool { return l.afterUpdate > 0 }

// handOver runs first in a process started by an update: the new version
// tells the old one it runs and waits for it to exit; a rolled-back old
// version waits for the failed new one.
func (l *launch) handOver(log *slog.Logger, dataDir string) error {
	switch {
	case l.afterUpdate > 0:
		if rt, err := instance.ReadRuntime(dataDir); err == nil && rt.PID == l.afterUpdate {
			l.from = rt.Version
		}
		if err := selfupdate.SignalReady(dataDir, os.Getpid()); err != nil {
			return fmt.Errorf("signal ready: %w", err)
		}
		log.Info("after update: ready; waiting for the old version to exit", "old_pid", l.afterUpdate, "from", l.from)
		if err := selfupdate.WaitPID(l.afterUpdate, oldExitWait); err != nil {
			return err
		}
		log.Info("after update: old version exited", "old_pid", l.afterUpdate)
	case l.rolledBack > 0:
		log.Warn("rolled back after a failed update; waiting for the failed version to exit", "pid", l.rolledBack)
		if err := selfupdate.WaitPID(l.rolledBack, 30*time.Second); err != nil {
			log.Error("rolled back: failed version still running", "err", err)
		}
	}
	return nil
}

// acquireLock takes the single-instance lock; right after an update the
// previous process may hold it for a moment more.
func acquireLock(dataDir string, l launch) (*instance.Lock, error) {
	lock, err := instance.Acquire(dataDir)
	if l.afterUpdate == 0 && l.rolledBack == 0 {
		return lock, err
	}
	deadline := time.Now().Add(lockWait)
	for errors.Is(err, instance.ErrAlreadyRunning) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		lock, err = instance.Acquire(dataDir)
	}
	if errors.Is(err, instance.ErrAlreadyRunning) {
		return nil, errors.New("the previous version still holds the data folder")
	}
	return lock, err
}

// rollbackUpdate runs in a new version that failed to start after an update
// (port taken, database error, …): it puts the old executable back, records
// why for the old version to show, and starts it. nil = handed back.
func rollbackUpdate(log *slog.Logger, dataDir, exe string, l launch, cause error) error {
	log.Error("after update: start failed; rolling back", "err", cause)
	res := selfupdate.Result{From: l.from, To: Version, Error: cause.Error(), At: time.Now().UTC()}
	if err := selfupdate.WriteResult(dataDir, res); err != nil {
		log.Error("rollback: write result", "err", err)
	}
	if err := selfupdate.Restore(exe); err != nil {
		return fmt.Errorf("%w; rollback failed: %w", cause, err)
	}
	if _, err := selfupdate.Start(exe, "--rolled-back="+strconv.Itoa(os.Getpid())); err != nil {
		return fmt.Errorf("%w; restarting the previous version failed: %w", cause, err)
	}
	log.Info("rollback: previous version started", "exe", exe)
	return nil
}

// newUpdater wires internal/selfupdate to the settings and the app.
func newUpdater(log *slog.Logger, cfgs *config.Store, dataDir, exe string, last *selfupdate.Result,
	onChange func(selfupdate.Status), quit func(),
) *selfupdate.Updater {
	return selfupdate.New(selfupdate.Options{
		Current: Version, Exe: exe, DataDir: dataDir, PID: os.Getpid(),
		Source: selfupdate.Source{APIBase: os.Getenv(envUpdateBase), UserAgent: "IssueWatcher/" + Version},
		Prefs: func() selfupdate.Prefs {
			u := cfgs.Get().Updates
			return selfupdate.Prefs{Channel: u.Channel, AutoCheck: u.AutoCheck, Interval: time.Duration(u.IntervalHours) * time.Hour}
		},
		Log: log, OnChange: onChange, Quit: quit, LastResult: last,
	})
}

// cleanupAfterUpdate deletes .old/.new next to exe once the process that ran
// them is gone (bounded retries) and the stale ready signal.
func cleanupAfterUpdate(log *slog.Logger, dataDir, exe string) {
	selfupdate.ClearReady(dataDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := selfupdate.Cleanup(ctx, exe, 40, 250*time.Millisecond); err != nil {
		log.Warn("update leftovers not removed", "err", err)
		return
	}
	log.Info("update leftovers removed")
}
