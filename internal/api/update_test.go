package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
)

func TestRequirePortFailsWhenTaken(t *testing.T) {
	var lc net.ListenConfig
	busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	port := busy.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // tcp listener
	old := requireWait
	requireWait = 300 * time.Millisecond
	defer func() { requireWait = old }()

	opts := Options{Assets: fstest.MapFS{}, PreferredPort: port, RequirePort: true, Log: slog.New(slog.DiscardHandler)}
	if _, err := New(t.Context(), opts); !errors.Is(err, ErrPortBusy) {
		t.Fatalf("taken required port: %v", err)
	}
	_ = busy.Close()
	s, err := New(t.Context(), opts)
	if err != nil || s.Port() != port {
		t.Fatalf("free required port: %v, port %d", err, s.Port())
	}
	_ = s.ln.Close()
}

type fakeUpdater struct{ installErr error }

func (f *fakeUpdater) Status() selfupdate.Status {
	return selfupdate.Status{Current: "v0.1.0", State: selfupdate.StateIdle}
}

func (f *fakeUpdater) Check(context.Context) (selfupdate.Status, error) {
	return selfupdate.Status{Current: "v0.1.0", UpdateAvailable: true}, nil
}
func (f *fakeUpdater) Install() error { return f.installErr }

func TestUpdateEndpoints(t *testing.T) {
	u := &fakeUpdater{}
	s, err := New(t.Context(), Options{Assets: fstest.MapFS{}, Log: slog.New(slog.DiscardHandler), Updates: u})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	post := func(path string) result {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, s.BaseURL()+path, nil)
		req.Header.Set("Authorization", "Bearer "+s.Token())
		return do(t, http.DefaultClient, req)
	}
	if r := get(t, http.DefaultClient, s.BaseURL()+"/api/update", s.Token()); r.status != 200 || !strings.Contains(r.body, `"current":"v0.1.0"`) {
		t.Fatalf("status: %+v", r)
	}
	if r := post("/api/update/check"); r.status != 200 || !strings.Contains(r.body, `"updateAvailable":true`) {
		t.Fatalf("check: %+v", r)
	}
	if r := post("/api/update/install"); r.status != http.StatusAccepted {
		t.Fatalf("install: %+v", r)
	}
	u.installErr = selfupdate.ErrDevBuild
	if r := post("/api/update/install"); r.status != http.StatusBadRequest {
		t.Fatalf("dev install: %+v", r)
	}
}
