package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// runEntry is the autostart Run value (internal/autostart.Entry), faked in headless runs.
type runEntry interface {
	Enabled(exe string) (bool, error)
	Set(on bool, exe string) error
}

// memEntry is an in-memory Run value: headless runs never write the registry.
type memEntry struct {
	mu sync.Mutex
	on bool
}

func (m *memEntry) Enabled(string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.on, nil
}

func (m *memEntry) Set(on bool, _ string) error {
	m.mu.Lock()
	m.on = on
	m.mu.Unlock()
	return nil
}

// runHeadless serves the API and runs the sync without any desktop UI until
// the process is interrupted or the server stops (IW_HEADLESS=1).
func runHeadless(syncCtx context.Context, log *slog.Logger, srv *api.Server, sy *syncer.Syncer) error {
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	go sy.Run(syncCtx)
	srv.OpenBrowser("/") // logs the launch URL (browser opening is suppressed)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	var err error
	select {
	case err = <-done:
		log.Error("http server stopped", "err", err)
	case s := <-sig:
		log.Info("headless: signal, exiting", "signal", s.String())
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(syncCtx), 3*time.Second)
	defer cancel()
	if serr := srv.Shutdown(ctx); serr != nil {
		log.Error("http shutdown", "err", serr)
	}
	log.Info("exit")
	return err
}
