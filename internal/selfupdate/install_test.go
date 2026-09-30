package selfupdate_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
	"github.com/UberMorgott/issuewatcher/internal/selfupdate/updatetest"
)

type env struct {
	src  selfupdate.Source
	fake *updatetest.Fake
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	exe  string
}

func setup(t *testing.T) *env {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fake := updatetest.NewFake()
	srv := fake.Start()
	t.Cleanup(srv.Close)
	exe := filepath.Join(t.TempDir(), "issuewatcher.exe")
	if err := os.WriteFile(exe, []byte("old exe"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &env{src: selfupdate.Source{APIBase: srv.URL}, fake: fake, pub: pub, priv: priv, exe: exe}
}

func (e *env) latest(t *testing.T, channel string) *selfupdate.Release {
	t.Helper()
	rel, err := e.src.Latest(t.Context(), channel)
	if err != nil || rel == nil {
		t.Fatalf("latest = %v, %v", rel, err)
	}
	return rel
}

func TestLatestChannels(t *testing.T) {
	e := setup(t)
	newExe := []byte("new exe")
	e.fake.Set(
		updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(e.priv, "v0.1.1", newExe, nil)},
		updatetest.Release{Tag: "v0.2.0-rc.1", Prerelease: true, Files: updatetest.Signed(e.priv, "v0.2.0-rc.1", newExe, nil)},
	)
	if got := e.latest(t, selfupdate.ChannelStable).Tag; got != "v0.1.1" {
		t.Errorf("stable = %s", got)
	}
	if got := e.latest(t, selfupdate.ChannelPreview).Tag; got != "v0.2.0-rc.1" {
		t.Errorf("preview = %s", got)
	}
	e.fake.Set()
	if rel, err := e.src.Latest(t.Context(), selfupdate.ChannelStable); rel != nil || err != nil {
		t.Errorf("no release = %v, %v", rel, err)
	}
}

func TestPrepareVerifiesAndSwaps(t *testing.T) {
	e := setup(t)
	newExe := bytes.Repeat([]byte("new exe "), 1000)
	e.fake.Set(updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(e.priv, "v0.1.1", newExe, nil)})
	var last int64
	m, err := e.src.Prepare(t.Context(), e.latest(t, "stable"), e.pub, "v0.1.0", e.exe, func(done, _ int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v0.1.1" || last != int64(len(newExe)) {
		t.Fatalf("manifest %+v, progress %d", m, last)
	}
	if err := selfupdate.Swap(e.exe); err != nil {
		t.Fatal(err)
	}
	assertFile(t, e.exe, newExe)
	assertFile(t, selfupdate.OldPath(e.exe), []byte("old exe"))

	// The new process failed: roll back.
	if err := selfupdate.Restore(e.exe); err != nil {
		t.Fatal(err)
	}
	assertFile(t, e.exe, []byte("old exe"))
	if err := selfupdate.Cleanup(t.Context(), e.exe, 3, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	assertGone(t, selfupdate.OldPath(e.exe), selfupdate.NewPath(e.exe))
}

func TestPrepareRefuses(t *testing.T) {
	newExe := []byte("new exe")
	flip := func(b []byte) []byte { b[0] ^= 0xff; return b }
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	for _, c := range []struct {
		name    string
		current string
		rel     func(e *env) updatetest.Release
		want    string
	}{
		{"bad sha, no GitHub digest", "v0.1.0", func(e *env) updatetest.Release {
			return updatetest.Release{Tag: "v0.1.1", NoDigest: true, Files: updatetest.Signed(e.priv, "v0.1.1", newExe, flip)}
		}, "checksum mismatch"},
		{"bad sha, GitHub digest", "v0.1.0", func(e *env) updatetest.Release {
			return updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(e.priv, "v0.1.1", newExe, flip)}
		}, "differs from the signed manifest"},
		{"wrong key", "v0.1.0", func(*env) updatetest.Release {
			return updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(otherKey, "v0.1.1", newExe, nil)}
		}, "signature"},
		{"downgrade", "v0.2.0", func(e *env) updatetest.Release {
			return updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(e.priv, "v0.1.1", newExe, nil)}
		}, "not newer"},
		{"replayed older manifest", "v0.1.0", func(e *env) updatetest.Release {
			return updatetest.Release{Tag: "v0.1.2", Files: updatetest.Signed(e.priv, "v0.1.1", newExe, nil)}
		}, "not the release"},
		{"unsigned", "v0.1.0", func(*env) updatetest.Release {
			return updatetest.Release{Tag: "v0.1.1", Files: map[string][]byte{selfupdate.ThisAsset(): newExe}}
		}, "no signed manifest"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.fake.Set(c.rel(e))
			_, err := e.src.Prepare(t.Context(), e.latest(t, "stable"), e.pub, c.current, e.exe, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			assertFile(t, e.exe, []byte("old exe"))
			assertGone(t, selfupdate.NewPath(e.exe), selfupdate.OldPath(e.exe))
		})
	}
}

func TestUpdaterCheckAndGuards(t *testing.T) {
	e := setup(t)
	e.fake.Set(updatetest.Release{Tag: "v0.1.1", Notes: "- fix", Files: updatetest.Signed(e.priv, "v0.1.1", []byte("x"), nil)})
	var events int
	newUpdater := func(current string) *selfupdate.Updater {
		return selfupdate.New(selfupdate.Options{
			Current: current, Exe: e.exe, DataDir: t.TempDir(), Source: e.src, PublicKey: e.pub,
			Prefs:    func() selfupdate.Prefs { return selfupdate.Prefs{Channel: "stable"} },
			Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			OnChange: func(selfupdate.Status) { events++ },
		})
	}
	u := newUpdater("v0.1.0")
	if err := u.Install(); !errors.Is(err, selfupdate.ErrNoUpdate) {
		t.Fatalf("install before check: %v", err)
	}
	st, err := u.Check(t.Context())
	if err != nil || !st.UpdateAvailable || st.Available.Version != "v0.1.1" || st.Available.Notes != "- fix" || events < 2 {
		t.Fatalf("check = %+v, %v, events %d", st, err, events)
	}
	dev := newUpdater("v0.1.0-2-gabcdef0")
	if st, _ := dev.Check(t.Context()); !st.DevBuild || st.UpdateAvailable {
		t.Fatalf("dev build status %+v", st)
	}
	if err := dev.Install(); !errors.Is(err, selfupdate.ErrDevBuild) {
		t.Fatalf("dev install: %v", err)
	}
}

// Concurrent Checks: exactly one reaches GitHub, the rest get ErrBusy (the
// idle check and the switch to checking are one step). The race window is
// short, so many rounds.
func TestUpdaterConcurrentChecks(t *testing.T) {
	var (
		hits    atomic.Int32
		release chan struct{}
		mu      sync.Mutex
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		mu.Lock()
		rel := release
		mu.Unlock()
		<-rel
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	const n = 8
	for round := range 200 {
		hits.Store(0)
		mu.Lock()
		release = make(chan struct{})
		mu.Unlock()
		u := selfupdate.New(selfupdate.Options{
			Current: "v0.1.0", Exe: filepath.Join(t.TempDir(), "x.exe"), DataDir: t.TempDir(), Source: selfupdate.Source{APIBase: srv.URL},
			Prefs: func() selfupdate.Prefs { return selfupdate.Prefs{Channel: "stable"} },
			Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		start := make(chan struct{})
		errs := make(chan error, n)
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				<-start
				_, err := u.Check(t.Context())
				errs <- err
			})
		}
		close(start)
		busy := 0
		for busy < n-1 && hits.Load() <= 1 { // the one Check that started waits in the handler
			select {
			case err := <-errs:
				if !errors.Is(err, selfupdate.ErrBusy) {
					close(release)
					t.Fatalf("round %d: a second Check ran: %v", round, err)
				}
				busy++
			case <-time.After(time.Millisecond):
			}
		}
		close(release)
		wg.Wait()
		if h := hits.Load(); busy != n-1 || h != 1 {
			t.Fatalf("round %d: %d of %d Checks got ErrBusy; GitHub hits %d", round, busy, n-1, h)
		}
		if err := <-errs; errors.Is(err, selfupdate.ErrBusy) {
			t.Fatalf("round %d: the running Check: %v", round, err)
		}
		if st := u.Status(); st.State != selfupdate.StateIdle {
			t.Fatalf("round %d: state after: %s", round, st.State)
		}
	}
}
func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // G304: test temp file
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, %v; want %q", filepath.Base(path), got, err, want)
	}
}

func assertGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists (%v)", filepath.Base(p), err)
		}
	}
}
