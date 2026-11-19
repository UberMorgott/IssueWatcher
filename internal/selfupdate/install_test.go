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
	return &env{src: selfupdate.Source{Base: srv.URL}, fake: fake, pub: pub, priv: priv, exe: exe}
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
		updatetest.Release{Tag: "v0.2.0-rc.1", Files: updatetest.Signed(e.priv, "v0.2.0-rc.1", newExe, nil)},
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

// The check reads github.com's releases feed, never the rate-limited REST API
// (anonymous quota 60/h per IP ran out and hid v0.21.2 from v0.21.1).
func TestLatestFromReleasesFeed(t *testing.T) {
	const feed = `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <entry>
    <updated>2026-11-20T10:00:00Z</updated>
    <link rel="alternate" type="text/html" href="https://github.com/UberMorgott/IssueWatcher/releases/tag/v0.22.0-rc.1"/>
    <title>IssueWatcher v0.22.0-rc.1</title>
    <content type="html">&lt;p&gt;rc&lt;/p&gt;</content>
  </entry>
  <entry>
    <updated>2026-11-19T16:05:26Z</updated>
    <link rel="alternate" type="text/html" href="https://github.com/UberMorgott/IssueWatcher/releases/tag/v0.21.2"/>
    <title>IssueWatcher v0.21.2</title>
    <content type="html">&lt;p&gt;Изменения:&lt;/p&gt;
&lt;ul&gt;
&lt;li&gt;Fix &lt;code&gt;gone&lt;/code&gt; &amp;amp; more&lt;/li&gt;
&lt;/ul&gt;</content>
  </entry>
</feed>`
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/UberMorgott/IssueWatcher/releases/latest" {
			http.Redirect(w, r, "/UberMorgott/IssueWatcher/releases/tag/v0.21.2", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(feed))
	}))
	t.Cleanup(srv.Close)
	src := selfupdate.Source{Base: srv.URL}
	rel, err := src.Latest(t.Context(), selfupdate.ChannelStable)
	if err != nil || rel == nil || rel.Tag != "v0.21.2" || rel.Prerelease || rel.Notes != "Изменения:\n- Fix gone & more" {
		t.Fatalf("stable = %+v, %v", rel, err)
	}
	if a := rel.Assets[0]; a.Name != selfupdate.ManifestAsset || a.URL != srv.URL+"/UberMorgott/IssueWatcher/releases/download/v0.21.2/manifest.json" {
		t.Fatalf("manifest asset %+v", a)
	}
	if rel, err := src.Latest(t.Context(), selfupdate.ChannelPreview); err != nil || rel.Tag != "v0.22.0-rc.1" || !rel.Prerelease {
		t.Fatalf("preview = %+v, %v", rel, err)
	}
	for _, p := range paths {
		if p != "/UberMorgott/IssueWatcher/releases.atom" && p != "/UberMorgott/IssueWatcher/releases/latest" {
			t.Fatalf("asked %s", p)
		}
	}
}

// Stable is what GitHub marks Latest: a release flagged prerelease with a
// plain tag is skipped, and a stable release older than the feed's newest
// entries is still found.
func TestLatestStableHonorsPrereleaseFlag(t *testing.T) {
	e := setup(t)
	exe := []byte("new exe")
	e.fake.Set(updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(e.priv, "v0.1.1", exe, nil)},
		updatetest.Release{Tag: "v0.1.2", Prerelease: true, Files: updatetest.Signed(e.priv, "v0.1.2", exe, nil)})
	if got := e.latest(t, selfupdate.ChannelStable); got.Tag != "v0.1.1" || got.Assets[0].URL != e.src.Base+"/UberMorgott/IssueWatcher/releases/download/v0.1.1/manifest.json" {
		t.Fatalf("stable = %+v", got)
	}

	// Feed without the stable tag (more than 10 newer prereleases).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.Redirect(w, r, "/UberMorgott/IssueWatcher/releases/tag/v0.1.0", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"><entry><link href="https://github.com/UberMorgott/IssueWatcher/releases/tag/v0.2.0-rc.11"/></entry></feed>`))
	}))
	t.Cleanup(srv.Close)
	if rel, err := (selfupdate.Source{Base: srv.URL}).Latest(t.Context(), selfupdate.ChannelStable); err != nil || rel == nil || rel.Tag != "v0.1.0" || rel.Prerelease {
		t.Fatalf("stable outside feed = %+v, %v", rel, err)
	}
}

// A 200 page that is not an Atom feed (a proxy's HTML) is a failed check, not
// "no releases".
func TestLatestRejectsNonAtomFeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body>Sign in to the proxy</body></html>`))
	}))
	t.Cleanup(srv.Close)
	if rel, err := (selfupdate.Source{Base: srv.URL}).Latest(t.Context(), selfupdate.ChannelPreview); err == nil {
		t.Fatalf("latest = %+v, want an error", rel)
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
		{"bad sha", "v0.1.0", func(e *env) updatetest.Release {
			return updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(e.priv, "v0.1.1", newExe, flip)}
		}, "checksum mismatch"},
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

// A failed check or install carries the code the UI translates.
func TestUpdaterErrorCodes(t *testing.T) {
	e := setup(t)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	e.fake.Set(updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(otherKey, "v0.1.1", []byte("x"), nil)})
	done := make(chan selfupdate.Status, 16)
	u := selfupdate.New(selfupdate.Options{
		Current: "v0.1.0", Exe: e.exe, DataDir: t.TempDir(), Source: e.src, PublicKey: e.pub,
		Prefs:    func() selfupdate.Prefs { return selfupdate.Prefs{Channel: "stable"} },
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnChange: func(st selfupdate.Status) { done <- st },
	})
	if _, err := u.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := u.Install(); err != nil {
		t.Fatal(err)
	}
	for st := range done {
		if st.State == selfupdate.StateIdle && st.Error != "" {
			if st.ErrorCode != selfupdate.CodeSignature {
				t.Fatalf("install code %q (%s)", st.ErrorCode, st.Error)
			}
			break
		}
	}
	bad := selfupdate.New(selfupdate.Options{
		Current: "v0.1.0", DataDir: t.TempDir(), Source: selfupdate.Source{Base: "http://127.0.0.1:9"},
		Prefs: func() selfupdate.Prefs { return selfupdate.Prefs{Channel: "stable"} },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if st, err := bad.Check(t.Context()); err == nil || st.ErrorCode != selfupdate.CodeCheckFailed {
		t.Fatalf("check = %+v, %v", st, err)
	}
}

// The last check survives a restart (also the restart into the new version),
// and a check of another channel is not reused.
func TestUpdaterLastCheckPersists(t *testing.T) {
	e := setup(t)
	e.fake.Set(updatetest.Release{Tag: "v0.1.1", Files: updatetest.Signed(e.priv, "v0.1.1", []byte("x"), nil)})
	dir := t.TempDir()
	channel := "stable"
	newUpdater := func(current string) *selfupdate.Updater {
		return selfupdate.New(selfupdate.Options{
			Current: current, Exe: e.exe, DataDir: dir, Source: e.src, PublicKey: e.pub,
			Prefs: func() selfupdate.Prefs { return selfupdate.Prefs{Channel: channel} },
			Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}
	if st := newUpdater("v0.1.0").Status(); st.CheckedAt != nil {
		t.Fatalf("fresh status has checkedAt %v", st.CheckedAt)
	}
	checked, err := newUpdater("v0.1.0").Check(t.Context())
	if err != nil || checked.CheckedAt == nil {
		t.Fatalf("check = %+v, %v", checked, err)
	}
	st := newUpdater("v0.1.1").Status()
	if st.CheckedAt == nil || !st.CheckedAt.Equal(*checked.CheckedAt) || st.Available == nil ||
		st.Available.Version != "v0.1.1" || st.UpdateAvailable {
		t.Fatalf("after restart = %+v", st)
	}
	channel = "preview"
	if st := newUpdater("v0.1.1").Status(); st.CheckedAt != nil || st.Available != nil {
		t.Fatalf("other channel reused the check: %+v", st)
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
			Current: "v0.1.0", Exe: filepath.Join(t.TempDir(), "x.exe"), DataDir: t.TempDir(), Source: selfupdate.Source{Base: srv.URL},
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
