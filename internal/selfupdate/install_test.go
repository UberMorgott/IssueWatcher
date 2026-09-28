package selfupdate_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
