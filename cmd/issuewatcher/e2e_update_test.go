//go:build e2e

// End-to-end self-update test: real executables, a fake GitHub releases API.
//
//	go test -tags e2e -run TestUpdateE2E -v -timeout 10m ./cmd/issuewatcher
//
// Builds v0.1.0 and v0.1.1 with an ephemeral release key, runs v0.1.0
// headless in scratch folders (own IW_DATA_DIR, IW_UPDATE_BASE → the fake) and
// installs v0.1.1 through the HTTP API. Every process it starts is stopped.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/instance"
	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
	"github.com/UberMorgott/issuewatcher/internal/selfupdate/updatetest"
)

type e2e struct {
	t     *testing.T
	root  string
	old   []byte // v0.1.0 exe
	new   []byte // v0.1.1 exe
	priv  ed25519.PrivateKey
	fake  *updatetest.Fake
	base  string
	mu    sync.Mutex
	procs []int // every PID seen, killed at the end
}

func build(t *testing.T, out, version, pub string) []byte {
	t.Helper()
	ld := fmt.Sprintf("-s -w -X main.Version=%s -X github.com/UberMorgott/issuewatcher/internal/selfupdate.publicKey=%s", version, pub)
	cmd := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-ldflags", ld, "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", version, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUpdateE2E(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("IW_E2E_DIR")
	if root == "" {
		root = t.TempDir()
	}
	root = filepath.Join(root, time.Now().Format("150405"))
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	e := &e2e{t: t, root: root, priv: priv, fake: updatetest.NewFake()}
	e.old = build(t, filepath.Join(root, "v0.1.0.exe"), "v0.1.0", pubB64)
	e.new = build(t, filepath.Join(root, "v0.1.1.exe"), "v0.1.1", pubB64)
	srv := e.fake.Start()
	defer srv.Close()
	e.base = srv.URL
	t.Cleanup(e.killAll)

	run := func(name string, f func(*testing.T)) {
		t.Run(name, func(t *testing.T) { e.t = t; f(t) })
	}
	run("success", e.success)
	run("corrupted", e.corrupted)
	run("busy port", e.busyPort)
}

// app is one portable install: exe + data dir.
type app struct {
	dir, exe, data string
	pid, port      int
	token          string
	jar            http.CookieJar
}

func (e *e2e) install(name string) *app {
	dir := filepath.Join(e.root, name)
	a := &app{dir: dir, exe: filepath.Join(dir, "issuewatcher.exe"), data: filepath.Join(dir, "data")}
	if err := os.MkdirAll(a.data, 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(a.exe, e.old, 0o755); err != nil { //nolint:gosec // test exe
		e.t.Fatal(err)
	}
	a.jar, _ = cookiejar.New(nil)
	return a
}

func (e *e2e) start(a *app) {
	cmd := exec.Command(a.exe) //nolint:gosec,noctx // our test build; outlives no test
	cmd.Env = append(os.Environ(), "IW_HEADLESS=1", "IW_DATA_DIR="+a.data, "IW_UPDATE_BASE="+e.base, "IW_PORT=")
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.track(cmd.Process.Pid)
	go func() { _ = cmd.Wait() }()
	e.waitRuntime(a, func(rt instance.Runtime) bool { return rt.PID == cmd.Process.Pid })
	e.signIn(a)
}

func (e *e2e) track(pid int) {
	e.mu.Lock()
	e.procs = append(e.procs, pid)
	e.mu.Unlock()
}

func (e *e2e) killAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, pid := range e.procs {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

// waitRuntime waits for data\runtime.json matching ok and answering /api/health.
func (e *e2e) waitRuntime(a *app, ok func(instance.Runtime) bool) instance.Runtime {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		rt, err := instance.ReadRuntime(a.data)
		if err == nil && ok(rt) {
			a.pid, a.port, a.token = rt.PID, rt.Port, rt.Token
			if v, err := a.health(); err == nil && v == rt.Version {
				e.track(rt.PID)
				return rt
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.t.Fatalf("runtime.json of %s never matched", a.dir)
	return instance.Runtime{}
}

func (a *app) url(p string) string { return "http://127.0.0.1:" + strconv.Itoa(a.port) + p }

func (a *app) call(method, p string) (int, []byte, error) {
	req, _ := http.NewRequestWithContext(context.Background(), method, a.url(p), nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (a *app) health() (string, error) {
	code, b, err := a.call(http.MethodGet, "/api/health")
	if err != nil || code != 200 {
		return "", fmt.Errorf("health %d %v", code, err)
	}
	var h struct{ Version string }
	return h.Version, json.Unmarshal(b, &h)
}

func (a *app) status(t *testing.T) selfupdate.Status {
	t.Helper()
	code, b, err := a.call(http.MethodGet, "/api/update")
	var st selfupdate.Status
	if err != nil || code != 200 || json.Unmarshal(b, &st) != nil {
		t.Fatalf("GET /api/update: %d %v %s", code, err, b)
	}
	return st
}

var launchRe = regexp.MustCompile(`url="?(http://127\.0\.0\.1:\d+/auth\?t=[0-9a-f]+)`)

// signIn opens the launch URL the headless start logged, like the browser
// would: the session cookie then signs the SSE client in across restarts.
func (e *e2e) signIn(a *app) {
	e.t.Helper()
	var u string
	for range 100 {
		b, _ := os.ReadFile(filepath.Join(a.data, "logs", "issuewatcher.log"))
		if m := launchRe.FindAllStringSubmatch(string(b), -1); len(m) > 0 {
			u = m[len(m)-1][1]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if u == "" {
		e.t.Fatal("no launch URL in the log")
	}
	c := &http.Client{Jar: a.jar, Timeout: 10 * time.Second}
	resp, err := c.Get(u) //nolint:noctx // test
	if err != nil {
		e.t.Fatal(err)
	}
	_ = resp.Body.Close()
}

// sse connects an event stream with the session cookie and sends event names.
func (a *app) sse(ctx context.Context) (<-chan string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.url("/api/events"), nil)
	resp, err := (&http.Client{Jar: a.jar}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("events: HTTP %d", resp.StatusCode)
	}
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if ev, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				ch <- ev
			}
		}
	}()
	return ch, nil
}

func (e *e2e) release(tag string, exe []byte, noDigest bool, tamper func([]byte) []byte, key ed25519.PrivateKey) {
	e.fake.Set(updatetest.Release{Tag: tag, Notes: "- e2e", NoDigest: noDigest, Files: updatetest.Signed(key, tag, exe, tamper)})
}

func (e *e2e) checkAndInstall(a *app) {
	e.t.Helper()
	if code, b, err := a.call(http.MethodPost, "/api/update/check"); err != nil || code != 200 || !bytes.Contains(b, []byte(`"updateAvailable":true`)) {
		e.t.Fatalf("check: %d %v %s", code, err, b)
	}
	if code, b, err := a.call(http.MethodPost, "/api/update/install"); err != nil || code != http.StatusAccepted {
		e.t.Fatalf("install: %d %v %s", code, err, b)
	}
}

func leftovers(a *app) []string {
	var out []string
	for _, p := range []string{selfupdate.OldPath(a.exe), selfupdate.NewPath(a.exe)} {
		if _, err := os.Stat(p); err == nil {
			out = append(out, filepath.Base(p))
		}
	}
	return out
}

func waitNoLeftovers(t *testing.T, a *app) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(leftovers(a)) == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("leftovers remain: %v", leftovers(a))
}

func assertExe(t *testing.T, a *app, want []byte, what string) {
	t.Helper()
	b, err := os.ReadFile(a.exe)
	if err != nil || !bytes.Equal(b, want) {
		t.Fatalf("%s: exe is not %s (%v)", a.exe, what, err)
	}
}

func (e *e2e) success(t *testing.T) {
	a := e.install("success")
	e.start(a)
	oldPID, port := a.pid, a.port
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events, err := a.sse(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.release("v0.1.1", e.new, false, nil, e.priv)
	start := time.Now()
	e.checkAndInstall(a)

	if err := selfupdate.WaitPID(oldPID, 60*time.Second); err != nil {
		t.Fatalf("old process: %v", err)
	}
	rt := e.waitRuntime(a, func(rt instance.Runtime) bool { return rt.PID != oldPID })
	t.Logf("v0.1.0 pid %d → v0.1.1 pid %d on port %d in %s", oldPID, rt.PID, rt.Port, time.Since(start).Round(time.Millisecond))
	if rt.Version != "v0.1.1" || rt.Port != port {
		t.Fatalf("new runtime %+v, want v0.1.1 on port %d", rt, port)
	}
	var sawRestart bool
	for ev := range events { // the old stream ends when the old process exits
		sawRestart = sawRestart || strings.Contains(ev, `"state":"restarting"`)
	}
	if !sawRestart {
		t.Error("the open tab never saw update.status restarting")
	}
	// The tab's EventSource reconnects with its persistent session cookie.
	if _, err := a.sse(ctx); err != nil {
		t.Fatalf("SSE reconnect after the update: %v", err)
	}
	waitNoLeftovers(t, a)
	assertExe(t, a, e.new, "v0.1.1")
	if st := a.status(t); st.LastResult == nil || !st.LastResult.OK || st.LastResult.From != "v0.1.0" || st.LastResult.To != "v0.1.1" {
		t.Fatalf("lastResult %+v", st.LastResult)
	}
	if st := a.status(t); st.UpdateAvailable {
		t.Fatal("v0.1.1 still offers an update")
	}
	e.stop(a)
}

func (e *e2e) stop(a *app) {
	if p, err := os.FindProcess(a.pid); err == nil {
		_ = p.Kill()
		_ = selfupdate.WaitPID(a.pid, 10*time.Second)
	}
}

// waitIdleError waits for the install to end in an error containing want.
func waitIdleError(t *testing.T, a *app, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st := a.status(t)
		if st.State == selfupdate.StateIdle && st.Error != "" {
			if !strings.Contains(st.Error, want) {
				t.Fatalf("error %q, want %q", st.Error, want)
			}
			t.Logf("refused: %s", st.Error)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("install never failed")
}

func (e *e2e) corrupted(t *testing.T) {
	a := e.install("corrupted")
	e.start(a)
	flip := func(b []byte) []byte { b[len(b)/2] ^= 0xff; return b }
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	for _, c := range []struct {
		name     string
		noDigest bool
		tamper   func([]byte) []byte
		key      ed25519.PrivateKey
		want     string
	}{
		{"bad sha", true, flip, e.priv, "checksum mismatch"},
		{"bad signature", false, nil, otherKey, "signature"},
	} {
		e.release("v0.1.1", e.new, c.noDigest, c.tamper, c.key)
		e.checkAndInstall(a)
		waitIdleError(t, a, c.want)
		if v, err := a.health(); err != nil || v != "v0.1.0" {
			t.Fatalf("%s: old version not serving: %q %v", c.name, v, err)
		}
		assertExe(t, a, e.old, "v0.1.0")
		if l := leftovers(a); len(l) > 0 {
			t.Fatalf("%s: leftovers %v", c.name, l)
		}
	}
	e.stop(a)
}

func (e *e2e) busyPort(t *testing.T) {
	a := e.install("busyport")
	e.start(a)
	oldPID := a.pid
	// Another program holds the port the app pinned: the new version must not
	// move to another port, it fails and rolls back.
	var lc net.ListenConfig
	busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	if err := instance.WritePort(a.data, busyPort); err != nil {
		t.Fatal(err)
	}
	e.release("v0.1.1", e.new, false, nil, e.priv)
	e.checkAndInstall(a)
	if err := selfupdate.WaitPID(oldPID, 60*time.Second); err != nil {
		t.Fatalf("old process: %v", err)
	}
	rt := e.waitRuntime(a, func(rt instance.Runtime) bool { return rt.PID != oldPID && rt.Version == "v0.1.0" })
	t.Logf("rolled back: v0.1.0 again as pid %d on port %d", rt.PID, rt.Port)
	waitNoLeftovers(t, a)
	assertExe(t, a, e.old, "v0.1.0")
	st := a.status(t)
	if st.LastResult == nil || st.LastResult.OK || !strings.Contains(st.LastResult.Error, "port") {
		t.Fatalf("lastResult %+v", st.LastResult)
	}
	t.Logf("rollback reason: %s", st.LastResult.Error)
	e.stop(a)
}
