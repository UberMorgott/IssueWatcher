package steamcmd

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/conpty"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/steamugc"
	"github.com/UberMorgott/issuewatcher/internal/tools"
)

// The test binary doubles as a fake steamcmd (FAKE_STEAMCMD=<state dir>),
// run through a real pseudo console. It records its arguments, what was
// typed into its console and the workshop VDF, and behaves by state files:
// "cached" = a cached sign-in; env FAKE_GUARD = "" | email | mobile;
// FAKE_EXPIRED = prompt (ask for a password instead of failing);
// FAKE_UPLOAD = fail.
func TestMain(m *testing.M) {
	if dir := os.Getenv("FAKE_STEAMCMD"); dir != "" {
		os.Exit(fakeSteamCMD(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

const (
	fakePassword = "hunter2-Secret"
	fakeCode     = "AB12C"
)

// appendFile appends s to name inside the fake's state dir.
func appendFile(dir, name, s string) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(s)
	_ = f.Close()
}

func fakeSteamCMD(dir string, args []string) int {
	appendFile(dir, "argv.txt", strings.Join(args, " ")+"\n")
	in := bufio.NewReader(os.Stdin)
	say := func(s string) { _, _ = os.Stdout.WriteString(s) }
	read := func() string {
		line, _ := in.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		appendFile(dir, "typed.txt", line+"\n")
		return line
	}
	if os.Getenv("FAKE_UPDATE") == "1" {
		// The first run of a fresh steamcmd.exe: the updater (its lines are
		// localized: the real ones of a Russian Windows, cf. the probe in
		// testdata/real_mobile_login) downloads, then starts the updated
		// steamcmd as its child in the same console and waits for it.
		say("Redirecting stderr to 'logs\\stderr.txt'\n[  0%] Проверка на наличие обновлений...\n[  0%] Загрузка обновления (0 из 43 472 КБ)...\n[ 50%] Загрузка обновления (21 166 из 43 472 КБ)...\n[100%] Загрузка обновления (43 472 из 43 472 КБ)...\n[----] Применение обновления...\n")
		time.Sleep(300 * time.Millisecond)
		cmd := exec.CommandContext(context.Background(), os.Args[0], args...) //nolint:gosec // G204: the test binary itself
		cmd.Env = append(os.Environ(), "FAKE_UPDATE=0")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				return ee.ExitCode()
			}
			return 9
		}
		return 0
	}
	say("Steam Console Client (c) Valve Corporation - version 1788292693\n-- type 'quit' to exit --\nLoading Steam API...OK\n\n")
	noPrompt := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "+@NoPromptForPassword":
			noPrompt = i+1 < len(args) && args[i+1] == "1"
			i++
		case "+@ShutdownOnFailedCommand":
			i++
		case "+login":
			i++
			if os.Getenv("FAKE_GUARD") == "mobile_real" {
				// Real steamcmd 1788292693 with a Steam Guard mobile
				// authenticator (testdata/real_mobile_login/console_log.txt):
				// the console stays at "Logging in user ..." while it waits;
				// only its connection log says "Waiting for confirmation".
				say("Cached credentials not found.\n\npassword: ")
				if read() != fakePassword {
					say("\nFAILED (Invalid Password)\n")
					return 5
				}
				say("\nProceeding with login using username/password.\nLogging in user '" + args[i] + "' [U:1:0] to Steam Public...")
				b, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_TESTDATA"), "real_mobile_login", "connection_log.txt"))
				_ = os.MkdirAll("logs", 0o700)
				appendFile("logs", "connection_log.txt", strings.ReplaceAll(string(b), "\r\n", "\n"))
				if os.Getenv("FAKE_APPROVE") == "never" {
					time.Sleep(time.Minute)
				}
				time.Sleep(1500 * time.Millisecond)
				appendFile(dir, "cached", "1")
				say("OK\nWaiting for client config...OK\nWaiting for user info...OK\n\nSteam>")
				if cmd := read(); cmd == "quit" {
					say("Unloading Steam API...OK\n")
					return 0
				}
				continue
			}
			say("Logging in user '" + args[i] + "' [U:1:35944863] to Steam Public...")
			if _, err := os.Stat(filepath.Join(dir, "cached")); err != nil {
				if noPrompt && os.Getenv("FAKE_EXPIRED") != "prompt" {
					say("\nCached credentials not found.\nFAILED (Invalid Password)\n")
					return 5
				}
				say("\npassword: ")
				if read() != fakePassword {
					say("\nFAILED (Invalid Password)\n")
					return 5
				}
				switch os.Getenv("FAKE_GUARD") {
				case "email":
					say("\nThis computer has not been authenticated for your account using Steam Guard.\nSteam Guard code:")
					if read() != fakeCode {
						say("\nFAILED (Invalid Login Auth Code)\n")
						return 5
					}
				case "mobile":
					say("\nThis account is protected by a Steam Guard mobile authenticator.\nPlease confirm the login in the Steam Mobile app on your phone.\n\nWaiting for confirmation...")
					time.Sleep(700 * time.Millisecond)
				}
				appendFile(dir, "cached", "1")
			}
			say("OK\nWaiting for client config...OK\nWaiting for user info...OK\n")
		case "+workshop_build_item":
			i++
			b, _ := os.ReadFile(args[i])
			appendFile(dir, "vdf.txt", string(b))
			say("Uploading content...\nPreparing update...\nCommitting update...\n")
			if os.Getenv("FAKE_UPLOAD") == "fail" {
				say("ERROR! Failed to update workshop item (Access Denied).\n")
				return 1
			}
			say("Success.\n")
		case "+quit":
			say("Unloading Steam API...OK\n")
			return 0
		}
	}
	for { // interactive: commands from the console
		say("Steam>")
		if cmd := read(); cmd == "quit" || cmd == "" {
			say("Unloading Steam API...OK\n")
			return 0
		}
	}
}

// fakeSteam is the Web API + community site for item 3739613434.
type fakeSteam struct {
	mu          sync.Mutex
	timeUpdated int64
	visibility  int
	notes       []changeNote // newest first
}

func (f *fakeSteam) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ISteamRemoteStorage/GetPublishedFileDetails/v1/", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("publishedfileids[0]") != "3739613434" || r.PostForm.Get("itemcount") != "1" {
			t.Errorf("details form %v", r.PostForm)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"response":{"result":1,"resultcount":1,"publishedfiledetails":[{"publishedfileid":"3739613434","result":1,"creator":"76561197996210591","creator_app_id":839770,"consumer_app_id":839770,"title":"Oracle","time_updated":%d,"visibility":%d,"banned":0}]}}`, f.timeUpdated, f.visibility)
	})
	mux.HandleFunc("GET /sharedfiles/filedetails/changelog/3739613434", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, n := range f.notes {
			_, _ = fmt.Fprintf(w, "<div class=\"detailBox workshopAnnouncement noFooter changeLogCtn\">\n<div class=\"changelog headline\">Update</div>\n<p id=\"%d\">%s</p>\n</div>\n", n.At, n.Text)
		}
	})
	return mux
}

type harness struct {
	w      *Workshop
	state  string // the fake's state dir
	data   string
	steam  *fakeSteam
	env    []string
	relog  atomic.Int32
	runs   atomic.Int32 // steamcmd starts
	up     *fakeUploader
	exeDir string
}

func newHarness(t *testing.T, env ...string) *harness {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("pseudo console: Windows only")
	}
	h := &harness{state: t.TempDir(), data: t.TempDir(), env: env, up: &fakeUploader{},
		steam: &fakeSteam{timeUpdated: 1787481191, notes: []changeNote{{At: 1787481191, Text: "v1.7.0 - drills"}, {At: 1783797336, Text: "v1.6.1 - Fix"}}}}
	srv := httptest.NewServer(h.steam.handler(t))
	t.Cleanup(srv.Close)
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	testdata, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	// The app's own steamcmd.exe (the runner starts the test binary).
	h.exeDir = filepath.Join(h.data, "tools")
	fakeExe := filepath.Join(h.exeDir, "steamcmd", "steamcmd.exe")
	if err := os.MkdirAll(filepath.Dir(fakeExe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeExe, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	h.w = New(Options{DataDir: h.data, Upload: h.up, HTTP: srv.Client(), APIURL: srv.URL, Site: srv.URL, Now: func() time.Time { return now },
		OnRelogin: func() { h.relog.Add(1) },
		Run: func(exe string, args []string, dir string) (Proc, error) {
			h.runs.Add(1)
			if exe != fakeExe || dir != filepath.Dir(fakeExe) {
				t.Errorf("runs %s in %s", exe, dir)
			}
			return conpty.Start(testExe, args, conpty.Options{Dir: dir, Env: append(append(os.Environ(), "FAKE_STEAMCMD="+h.state, "FAKE_TESTDATA="+testdata), h.env...)})
		},
		LoginTimeout: 30 * time.Second, CheckTimeout: 30 * time.Second, UploadTimeout: 30 * time.Second, LogPoll: 50 * time.Millisecond})
	return h
}

func (h *harness) file(name string) string {
	root, err := os.OpenRoot(h.state)
	if err != nil {
		return ""
	}
	defer func() { _ = root.Close() }()
	b, _ := root.ReadFile(name)
	return string(b)
}

// waitLogin polls the sign-in until it reaches one of states.
func (h *harness) waitLogin(t *testing.T, states ...string) LoginState {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, err := h.w.Status()
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(states, st.Login.State) {
			return st.Login
		}
		time.Sleep(50 * time.Millisecond)
	}
	st, _ := h.w.Status()
	t.Fatalf("sign-in state %+v, want %v", st.Login, states)
	return LoginState{}
}

func TestLoginTypesPasswordAndGuardCodeIntoTheConsole(t *testing.T) {
	h := newHarness(t, "FAKE_GUARD=email")
	if _, err := h.w.StartLogin("owner_acc", fakePassword, ""); err != nil {
		t.Fatal(err)
	}
	h.waitLogin(t, LoginNeedCode)
	if _, err := h.w.SubmitCode(fakeCode); err != nil {
		t.Fatal(err)
	}
	h.waitLogin(t, LoginOK)
	if got := h.file("argv.txt"); got != "+login owner_acc\n" {
		t.Fatalf("argv %q", got)
	}
	if got := h.file("typed.txt"); got != fakePassword+"\n"+fakeCode+"\nquit\n" {
		t.Fatalf("typed %q", got)
	}
	st, _ := h.w.Status()
	if !st.LoggedIn || st.User != "owner_acc" || st.Tool.State != tools.Ready || !strings.HasSuffix(st.SteamCMD, `steamcmd\steamcmd.exe`) {
		t.Fatalf("status %+v", st)
	}
	b, _ := os.ReadFile(filepath.Join(h.data, "secrets", settingsFile))
	if bytes.Contains(b, []byte(fakePassword)) || bytes.Contains(b, []byte("owner_acc")) {
		t.Fatal("settings not protected / password stored")
	}
	// The cached sign-in works for a check.
	if _, err := h.w.CheckSession(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLoginCodeUpFrontAndMobileApproval(t *testing.T) {
	h := newHarness(t, "FAKE_GUARD=email")
	if _, err := h.w.StartLogin("owner_acc", fakePassword, fakeCode); err != nil {
		t.Fatal(err)
	}
	h.waitLogin(t, LoginOK)

	m := newHarness(t, "FAKE_GUARD=mobile")
	if _, err := m.w.StartLogin("owner_acc", fakePassword, ""); err != nil {
		t.Fatal(err)
	}
	m.waitLogin(t, LoginConfirm, LoginOK)
	m.waitLogin(t, LoginOK)
}

func TestLoginWrongPasswordFails(t *testing.T) {
	h := newHarness(t)
	if _, err := h.w.StartLogin("owner_acc", "wrong-password", ""); err != nil {
		t.Fatal(err)
	}
	st := h.waitLogin(t, LoginFailed)
	if st.Code != FailInvalidPassword || !strings.Contains(st.Error, "Invalid Password") || strings.Contains(st.Error, "wrong-password") {
		t.Fatalf("failure %q", st.Error)
	}
	if s, _ := h.w.Status(); s.LoggedIn || s.User != "" {
		t.Fatalf("stored a failed sign-in: %+v", s)
	}
	if _, err := h.w.StartLogin("bad user!", "x", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("bad user: %v", err)
	}
}

// TestLoginRealMobileApproval replays the owner's real first sign-in (v0.15.1
// hung on "steamcmd запускается и входит…"): a fresh steamcmd updates itself
// and restarts as its own child in the same console, then waits silently for
// the Steam Mobile approval, which only its connection log mentions.
func TestLoginRealMobileApproval(t *testing.T) {
	h := newHarness(t, "FAKE_GUARD=mobile_real", "FAKE_UPDATE=1")
	if _, err := h.w.StartLogin("owner_acc", fakePassword, ""); err != nil {
		t.Fatal(err)
	}
	var seen []string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := h.w.Status()
		if s := st.Login.State; len(seen) == 0 || seen[len(seen)-1] != s {
			seen = append(seen, s)
		}
		if st.Login.State == LoginOK || st.Login.State == LoginFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := []string{LoginUpdating, LoginLoggingIn, LoginConfirm, LoginOK}
	i := 0
	for _, s := range seen {
		if i < len(want) && s == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("states %v, want in order %v", seen, want)
	}
	if got := h.file("typed.txt"); got != fakePassword+"\nquit\n" {
		t.Fatalf("typed %q", got)
	}
	if st, _ := h.w.Status(); !st.LoggedIn || st.User != "owner_acc" {
		t.Fatalf("status %+v", st)
	}
}

// TestLoginMobileNotApprovedTimesOut: no endless spinner; a reason instead.
func TestLoginMobileNotApprovedTimesOut(t *testing.T) {
	h := newHarness(t, "FAKE_GUARD=mobile_real", "FAKE_APPROVE=never")
	h.w.opts.LoginTimeout = 2 * time.Second
	if _, err := h.w.StartLogin("owner_acc", fakePassword, ""); err != nil {
		t.Fatal(err)
	}
	st := h.waitLogin(t, LoginFailed)
	if st.Code != FailTimeoutMobile || !strings.Contains(st.Error, "Steam Mobile") {
		t.Fatalf("failure %+v", st)
	}
}

// TestAwaitsMobileReadsOnlyThisRun: the connection log grows across runs; an
// older run's "Waiting for confirmation" does not count.
func TestAwaitsMobileReadsOnlyThisRun(t *testing.T) {
	dir := t.TempDir()
	p := connLog(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	old := "[2026-10-05 15:52:06] Waiting for confirmation\n"
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	w := New(Options{DataDir: t.TempDir()})
	l := &loginRun{dir: dir, logOff: logSize(p)}
	if w.awaitsMobile(l) {
		t.Fatal("an older run's line counted")
	}
	real, err := os.ReadFile(filepath.Join("testdata", "real_mobile_login", "connection_log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append([]byte(old), real...), 0o600); err != nil {
		t.Fatal(err)
	}
	if !w.awaitsMobile(l) {
		t.Fatal("the real connection log's wait not seen")
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil { // started anew
		t.Fatal(err)
	}
	if w.awaitsMobile(l) {
		t.Fatal("a fresh log without the wait counted")
	}
}

func TestLoginPhaseAndFailureCodes(t *testing.T) {
	// The real console of a fresh steamcmd on a Russian Windows (conpty probe).
	probe := "Redirecting stderr to 'E:\\x\\logs\\stderr.txt'\nLogging directory: 'E:\\x/logs'\n[  0%] Проверка на наличие обновлений...\n[----] Проверка установки...\n"
	if p := phase(probe); p != LoginUpdating {
		t.Fatalf("update phase %q", p)
	}
	if p := phase(probe + "Steam Console Client (c) Valve Corporation - version 1788292693\nLoading Steam API...OK\n"); p != LoginStarting {
		t.Fatalf("start phase %q", p)
	}
	if p := phase(probe + "Loading Steam API...OK\nLogging in user 'owner_acc' [U:1:0] to Steam Public..."); p != LoginLoggingIn {
		t.Fatalf("login phase %q", p)
	}
	for line, want := range map[string]string{
		"FAILED (Invalid Password)":         FailInvalidPassword,
		"FAILED (Rate Limit Exceeded)":      FailRateLimit,
		"FAILED (Invalid Login Auth Code)":  FailBadCode,
		"FAILED (Two-factor code mismatch)": FailBadCode,
		"FAILED (Account Logon Denied)":     FailLogonDenied,
		"FAILED (No Connection)":            FailNetwork,
		"ERROR (Timeout)":                   FailNetwork,
		"FAILED (Account Disabled)":         FailSteam,
	} {
		if got := failCode(line); got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}

// contentZip writes a mod archive and returns its path.
func contentZip(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "oracle_1.7.1.zip")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Create("oracle_1.7.1.zip")
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{"meta.json": `{"version":"1.7.1"}`, "Oracle.dll": "MZ dll"} {
		w, _ := zw.Create(name)
		_, _ = io.WriteString(w, body)
	}
	_ = zw.Close()
	_ = f.Close()
	return p
}

var item = provider.Project{ExternalID: "3739613434"}

// fakeUploader is the Steam client uploader (steamugc.Client): it records
// each upload and the content folder's files at that moment.
type fakeUploader struct {
	mu    sync.Mutex
	calls []upCall
	err   error
	ready error
}

type upCall struct {
	app       uint32
	item      uint64
	dir, note string
	files     []string
}

func (f *fakeUploader) Upload(_ context.Context, app uint32, item uint64, dir, note string, progress func(steamugc.UploadProgress)) (steamugc.UploadResult, error) {
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(files)
	f.mu.Lock()
	f.calls = append(f.calls, upCall{app: app, item: item, dir: dir, note: note, files: files})
	err := f.err
	f.mu.Unlock()
	progress(steamugc.UploadProgress{Status: 3, Processed: 4, Total: 8})
	return steamugc.UploadResult{Item: item, EResult: 1}, err
}

func (f *fakeUploader) Ready() error { return f.ready }

func (f *fakeUploader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestPublishThroughTheSteamClient: the upload goes through the running
// Steam client (the uploader) with the unpacked content and the change note;
// steamcmd never runs and no steamcmd sign-in is needed.
func TestPublishThroughTheSteamClient(t *testing.T) {
	h := newHarness(t)
	zp := contentZip(t)
	req := provider.PublishRequest{Path: zp, Version: "1.7.1", Name: "Oracle", Changelog: `Fixes #3: "quoted"`}

	dry := req
	dry.DryRun = true
	res, err := h.w.Publish(context.Background(), item, dry, nil)
	if err != nil || len(res.Plan) != 1 || strings.Contains(res.Plan[0].Note, "not ready") || h.up.count() != 0 {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	var steps []provider.PublishProgress
	res, err = h.w.Publish(context.Background(), item, req, func(p provider.PublishProgress) { steps = append(steps, p) })
	if err != nil || res.VersionID != "1.7.1" || !strings.HasPrefix(res.FileName, "2 files, content sha256 ") {
		t.Fatalf("publish: %+v %v", res, err)
	}
	content := filepath.Join(h.data, "tools", "steamugc", "content", "3739613434")
	if h.up.count() != 1 {
		t.Fatalf("uploads %d", h.up.count())
	}
	c := h.up.calls[0]
	if c.app != 839770 || c.item != 3739613434 || c.dir != content || c.note != "v1.7.1 - Fixes #3: 'quoted'" || strings.Join(c.files, ",") != "Oracle.dll,meta.json" {
		t.Fatalf("upload %+v", c)
	}
	if last := steps[len(steps)-1]; last.Stage != provider.StagePublish || last.Sent != 4 || last.Total != 8 {
		t.Fatalf("progress %+v", steps)
	}
	if n := h.runs.Load(); n != 0 || h.file("argv.txt") != "" {
		t.Fatalf("steamcmd ran %d times", n)
	}

	// Available once the change note is listed and the item updated.
	h.steam.mu.Lock()
	h.steam.notes = append([]changeNote{{At: 1790000000, Text: "v1.7.1 - Fixes #3: 'quoted'"}}, h.steam.notes...)
	h.steam.mu.Unlock()
	pt, err := h.w.PublishTargets(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	vs := pt.Files[0].Versions
	if last := vs[len(vs)-1]; last.Version != "1.7.1" || !last.Pending {
		t.Fatalf("before time_updated: %+v", vs)
	}
	h.steam.mu.Lock()
	h.steam.timeUpdated = 1790000000
	h.steam.mu.Unlock()
	pt, _ = h.w.PublishTargets(context.Background(), item)
	vs = pt.Files[0].Versions
	if last := vs[len(vs)-1]; last.Version != "1.7.1" || last.Pending || last.ID != "1790000000" || len(vs) != 3 {
		t.Fatalf("available: %+v", vs)
	}
	// The same version again is refused before anything is sent.
	if _, err := h.w.Publish(context.Background(), item, req, nil); !errors.Is(err, provider.ErrBadPublish) || h.up.count() != 1 {
		t.Fatalf("republish: %v", err)
	}
	if err := h.w.CheckPublish(item, provider.PublishRequest{Path: zp, Version: "1.7.2", AppID: 1}); err != nil {
		t.Fatal(err) // appId mismatch is found at publish time
	}
	if _, err := h.w.Publish(context.Background(), item, provider.PublishRequest{Path: zp, Version: "1.7.2", AppID: 1}, nil); !errors.Is(err, provider.ErrBadPublish) {
		t.Fatalf("app id mismatch: %v", err)
	}
	if n := h.runs.Load(); n != 0 {
		t.Fatalf("steamcmd ran %d times", n)
	}
}

// TestPublishSteamClientFailures: Steam offline is an upload-auth error at
// resolve (nothing sent: the release holds auth:steam); a refusal or a lost
// answer fails the publish stage (sent: the probe decides); a missing
// steam_api64.dll refuses before anything is sent. steamcmd never runs.
func TestPublishSteamClientFailures(t *testing.T) {
	for _, c := range []struct {
		name       string
		err, ready error
		stage      string
		auth       bool
		uploads    int
	}{
		{"offline", fmt.Errorf("%w (Steam API init failed)", steamugc.ErrSteamOffline), nil, provider.StageResolve, true, 1},
		{"refused", fmt.Errorf("%w: SubmitItemUpdate: EResult 15 (AccessDenied)", steamugc.ErrSteam), nil, provider.StagePublish, false, 1},
		{"unknown", fmt.Errorf("%w: no answer", steamugc.ErrUploadUnknown), nil, provider.StagePublish, false, 1},
		{"no dll", nil, steamugc.ErrNoSteamAPI, provider.StageResolve, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.up.err, h.up.ready = c.err, c.ready
			_, err := h.w.Publish(context.Background(), item, provider.PublishRequest{Path: contentZip(t), Version: "1.7.1"}, nil)
			var pe *provider.PublishError
			if !errors.As(err, &pe) || pe.Stage != c.stage || errors.Is(err, provider.ErrNoUploadAuth) != c.auth || h.up.count() != c.uploads {
				t.Fatalf("%v (stage %+v) uploads %d", err, pe, h.up.count())
			}
			if h.runs.Load() != 0 {
				t.Fatal("steamcmd ran")
			}
		})
	}
}

// TestPublishOutcomeUnknownThenProbe: a lost answer is not retried by the
// publisher; the probe (PublishTargets) finds the version once its change
// note is listed, and a retry is then refused before anything is sent.
func TestPublishOutcomeUnknownThenProbe(t *testing.T) {
	h := newHarness(t)
	h.up.err = fmt.Errorf("%w: steam helper: no answer", steamugc.ErrUploadUnknown)
	if _, err := h.w.Publish(context.Background(), item, provider.PublishRequest{Path: contentZip(t), Version: "1.7.1"}, nil); !errors.Is(err, steamugc.ErrUploadUnknown) {
		t.Fatalf("lost answer: %v", err)
	}
	h.steam.mu.Lock()
	h.steam.notes = append([]changeNote{{At: 1790000000, Text: "v1.7.1"}}, h.steam.notes...)
	h.steam.mu.Unlock()
	pt, err := h.w.PublishTargets(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if vs := pt.Files[0].Versions; vs[len(vs)-1].Version != "1.7.1" {
		t.Fatalf("probe: %+v", vs)
	}
	if _, err := h.w.Publish(context.Background(), item, provider.PublishRequest{Path: contentZip(t), Version: "1.7.1"}, nil); !errors.Is(err, provider.ErrBadPublish) || h.up.count() != 1 {
		t.Fatalf("retry after the probe: %v uploads %d", err, h.up.count())
	}
}

func TestParseRealChangeNotes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "changelog.html"))
	if err != nil {
		t.Fatal(err)
	}
	ns := parseChangeNotes(string(b))
	if len(ns) != 3 || ns[0].At != 1787481191 || noteVersion(ns[0].Text) != "1.7.0" || noteVersion(ns[1].Text) != "1.7.0" || noteVersion(ns[2].Text) != "1.6.1" {
		t.Fatalf("notes %+v", ns)
	}
	if !strings.Contains(ns[2].Text, "'<!-MISSING KEY") {
		t.Fatalf("html entities: %q", ns[2].Text)
	}
}

func TestInstallDownloadsSteamCMD(t *testing.T) {
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("steamcmd.exe")
	_, _ = io.WriteString(w, "MZ steamcmd")
	_ = zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client/installer/steamcmd.zip" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(zb.Bytes())
	}))
	defer srv.Close()
	data := t.TempDir()
	var warmed []string
	ws := New(Options{DataDir: data, HTTP: srv.Client(), Download: srv.URL + "/client/installer/steamcmd.zip",
		Run: func(exe string, args []string, _ string) (Proc, error) {
			warmed = append(warmed, filepath.Base(exe)+" "+strings.Join(args, " "))
			return nil, errors.New("not a real steamcmd")
		}})
	if st, _ := ws.Status(); st.SteamCMD != "" {
		t.Fatalf("found before install: %+v", st)
	}
	st, err := ws.Install(context.Background())
	if err != nil || st.Tool.State != tools.Ready || st.SteamCMD != filepath.Join(data, "tools", "steamcmd", "steamcmd.exe") {
		t.Fatalf("install: %+v %v", st, err)
	}
	// The fresh steamcmd runs once with +quit (its self-update) right away;
	// a failure there does not fail the install.
	if !slices.Equal(warmed, []string{"steamcmd.exe +quit"}) {
		t.Fatalf("warm-up runs %q", warmed)
	}
	if b, _ := os.ReadFile(st.SteamCMD); string(b) != "MZ steamcmd" {
		t.Fatalf("exe %q", b)
	}
}

// TestProvisionDownloadsOnceAndCarriesTheSignInOver: the startup provisioning
// downloads steamcmd into data\tools\steamcmd once, copies the sign-in cache
// of the steamcmd an older version used (settings path) and drops that path;
// a failed download shows in the tool status and the next attempt retries.
func TestProvisionDownloadsOnceAndCarriesTheSignInOver(t *testing.T) {
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("steamcmd.exe")
	_, _ = io.WriteString(w, "MZ steamcmd")
	_ = zw.Close()
	var hits atomic.Int32
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(zb.Bytes())
	}))
	defer srv.Close()
	data := t.TempDir()
	// An older version's steamcmd outside the app folder with a cached sign-in.
	old := filepath.Join(t.TempDir(), "steamcmd")
	if err := os.MkdirAll(filepath.Join(old, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"steamcmd.exe": "MZ old", filepath.Join("config", "config.vdf"): `"InstallConfigStore" { "ConnectCache" { "x" "y" } }`} {
		if err := os.WriteFile(filepath.Join(old, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws := New(Options{DataDir: data, HTTP: srv.Client(), Download: srv.URL})
	if _, err := ws.update(func(s *settings) { s.User, s.Path, s.Found = "owner_acc", filepath.Join(old, "steamcmd.exe"), true }); err != nil {
		t.Fatal(err)
	}
	if exe, err := ws.steamcmd(); !errors.Is(err, ErrNoSteamCMD) || exe != "" {
		t.Fatalf("the old steamcmd must not be used: %q %v", exe, err)
	}
	down.Store(true)
	if _, err := ws.Provision(context.Background()); err == nil {
		t.Fatal("a failed download reports no error")
	}
	if st := ws.Tool(); st.State != tools.Failed || !strings.Contains(st.Error, "HTTP 503") {
		t.Fatalf("failed download: %+v", st)
	}
	down.Store(false)
	if _, err := ws.Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(data, "tools", "steamcmd", "steamcmd.exe")
	st, err := ws.Status()
	if err != nil || st.Tool.State != tools.Ready || st.SteamCMD != exe || st.Tool.Path != exe || st.User != "owner_acc" {
		t.Fatalf("after provisioning: %+v %v", st, err)
	}
	root, err := os.OpenRoot(data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if b, _ := root.ReadFile(filepath.Join("tools", "steamcmd", "config", "config.vdf")); !strings.Contains(string(b), "ConnectCache") {
		t.Fatalf("sign-in cache not carried over: %q", b)
	}
	if s, _ := ws.load(); s.Path != "" || s.Found {
		t.Fatalf("old path kept: %+v", s)
	}
	if st, err := ws.Provision(context.Background()); err != nil || st.State != tools.Ready {
		t.Fatalf("again: %+v %v", st, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("downloads %d, want 2 (one failed, one ok, none once ready)", hits.Load())
	}
}
