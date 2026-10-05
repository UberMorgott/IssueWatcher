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
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/conpty"
	"github.com/UberMorgott/issuewatcher/internal/provider"
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

func appendFile(path, s string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(s)
	_ = f.Close()
}

func fakeSteamCMD(dir string, args []string) int {
	appendFile(filepath.Join(dir, "argv.txt"), strings.Join(args, " ")+"\n")
	in := bufio.NewReader(os.Stdin)
	say := func(s string) { _, _ = os.Stdout.WriteString(s) }
	read := func() string {
		line, _ := in.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		appendFile(filepath.Join(dir, "typed.txt"), line+"\n")
		return line
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
				appendFile(filepath.Join(dir, "cached"), "1")
			}
			say("OK\nWaiting for client config...OK\nWaiting for user info...OK\n")
		case "+workshop_build_item":
			i++
			b, _ := os.ReadFile(args[i])
			appendFile(filepath.Join(dir, "vdf.txt"), string(b))
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
	exeDir string
}

func newHarness(t *testing.T, env ...string) *harness {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("pseudo console: Windows only")
	}
	h := &harness{state: t.TempDir(), data: t.TempDir(), env: env,
		steam: &fakeSteam{timeUpdated: 1787481191, notes: []changeNote{{At: 1787481191, Text: "v1.7.0 - drills"}, {At: 1783797336, Text: "v1.6.1 - Fix"}}}}
	srv := httptest.NewServer(h.steam.handler(t))
	t.Cleanup(srv.Close)
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A steamcmd.exe the publisher finds (the runner starts the test binary).
	h.exeDir = t.TempDir()
	fakeExe := filepath.Join(h.exeDir, "steamcmd", "steamcmd.exe")
	if err := os.MkdirAll(filepath.Dir(fakeExe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeExe, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	h.w = New(Options{DataDir: h.data, HTTP: srv.Client(), APIURL: srv.URL, Site: srv.URL, Now: func() time.Time { return now },
		Candidates: func() []string { return []string{filepath.Join(h.exeDir, "missing", "steamcmd.exe"), fakeExe} },
		OnRelogin:  func() { h.relog.Add(1) },
		Run: func(exe string, args []string, dir string) (Proc, error) {
			if exe != fakeExe || dir != filepath.Dir(fakeExe) {
				t.Errorf("runs %s in %s", exe, dir)
			}
			return conpty.Start(testExe, args, conpty.Options{Dir: dir, Env: append(append(os.Environ(), "FAKE_STEAMCMD="+h.state), h.env...)})
		},
		LoginTimeout: 30 * time.Second, CheckTimeout: 30 * time.Second, UploadTimeout: 30 * time.Second})
	return h
}

func (h *harness) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.state, name))
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
		for _, s := range states {
			if st.Login.State == s {
				return st.Login
			}
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
	if !st.LoggedIn || st.User != "owner_acc" || st.Source != "found" || !strings.HasSuffix(st.SteamCMD, `steamcmd\steamcmd.exe`) {
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
	if !strings.Contains(st.Error, "Invalid Password") || strings.Contains(st.Error, "wrong-password") {
		t.Fatalf("failure %q", st.Error)
	}
	if s, _ := h.w.Status(); s.LoggedIn || s.User != "" {
		t.Fatalf("stored a failed sign-in: %+v", s)
	}
	if _, err := h.w.StartLogin("bad user!", "x", ""); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("bad user: %v", err)
	}
}

// contentZip writes a mod archive and returns its path.
func contentZip(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "oracle_1.7.1.zip")
	f, err := os.Create(p)
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

func signedIn(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.w.StartLogin("owner_acc", fakePassword, ""); err != nil {
		t.Fatal(err)
	}
	h.waitLogin(t, LoginOK)
	_ = os.Remove(filepath.Join(h.state, "argv.txt"))
}

var item = provider.Project{ExternalID: "3739613434"}

func TestPublishExactVDFAndArgs(t *testing.T) {
	h := newHarness(t)
	zp := contentZip(t)
	req := provider.PublishRequest{Path: zp, Version: "1.7.1", Name: "Oracle", Changelog: `Fixes #3: "quoted"`}
	if _, err := h.w.Publish(context.Background(), item, req, nil); !errors.Is(err, ErrNoLogin) || !errors.Is(err, provider.ErrNoUploadAuth) {
		t.Fatalf("no sign-in: %v", err)
	}
	signedIn(t, h)

	dry := req
	dry.DryRun = true
	res, err := h.w.Publish(context.Background(), item, dry, nil)
	if err != nil || len(res.Plan) != 1 || h.file("argv.txt") != "" {
		t.Fatalf("dry run: %+v %v argv %q", res, err, h.file("argv.txt"))
	}
	res, err = h.w.Publish(context.Background(), item, req, nil)
	if err != nil || res.VersionID != "1.7.1" || !strings.HasPrefix(res.FileName, "2 files, content sha256 ") {
		t.Fatalf("publish: %+v %v", res, err)
	}
	vdfPath := filepath.Join(h.data, "tools", "steamcmd", "builds", "3739613434.vdf")
	if got, want := h.file("argv.txt"), "+@ShutdownOnFailedCommand 1 +@NoPromptForPassword 1 +login owner_acc +workshop_build_item "+vdfPath+" +quit\n"; got != want {
		t.Fatalf("argv\n%q\nwant\n%q", got, want)
	}
	content := filepath.ToSlash(filepath.Join(h.data, "tools", "steamcmd", "content", "3739613434"))
	want := "\"workshopitem\"\n{\n\t\"appid\"\t\t\"839770\"\n\t\"publishedfileid\"\t\t\"3739613434\"\n\t\"contentfolder\"\t\t\"" + content +
		"\"\n\t\"changenote\"\t\t\"v1.7.1 - Fixes #3: 'quoted'\"\n}\n"
	if got := h.file("vdf.txt"); got != want {
		t.Fatalf("vdf\n%s\nwant\n%s", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(filepath.FromSlash(content), "Oracle.dll")); err != nil || string(b) != "MZ dll" {
		t.Fatalf("content: %q %v", b, err)
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
	// The same version again is refused before steamcmd runs.
	_ = os.Remove(filepath.Join(h.state, "argv.txt"))
	if _, err := h.w.Publish(context.Background(), item, req, nil); !errors.Is(err, provider.ErrBadPublish) || h.file("argv.txt") != "" {
		t.Fatalf("republish: %v", err)
	}
	if err := h.w.CheckPublish(item, provider.PublishRequest{Path: zp, Version: "1.7.2", AppID: 1}); err != nil {
		t.Fatal(err) // appId mismatch is found at publish time
	}
	if _, err := h.w.Publish(context.Background(), item, provider.PublishRequest{Path: zp, Version: "1.7.2", AppID: 1}, nil); !errors.Is(err, provider.ErrBadPublish) {
		t.Fatalf("app id mismatch: %v", err)
	}
}

func TestPublishExpiredSignInIsRelogin(t *testing.T) {
	for _, mode := range []string{"fail", "prompt"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, "FAKE_EXPIRED="+mode)
			signedIn(t, h)
			_ = os.Remove(filepath.Join(h.state, "cached")) // steamcmd lost its cached sign-in
			req := provider.PublishRequest{Path: contentZip(t), Version: "1.7.1", Name: "Oracle"}
			_, err := h.w.Publish(context.Background(), item, req, nil)
			if !errors.Is(err, ErrRelogin) || !errors.Is(err, provider.ErrUploadAuthRefused) {
				t.Fatalf("expired: %v", err)
			}
			if h.file("vdf.txt") != "" {
				t.Fatal("uploaded without a sign-in")
			}
			// Known expired: refused without running steamcmd, still one notification.
			_, err = h.w.Publish(context.Background(), item, req, nil)
			if !errors.Is(err, ErrRelogin) || h.relog.Load() != 1 {
				t.Fatalf("second: %v, notifications %d", err, h.relog.Load())
			}
			if st, _ := h.w.Status(); st.LoggedIn || !st.Expired {
				t.Fatalf("status %+v", st)
			}
			if _, err := h.w.CheckSession(context.Background()); !errors.Is(err, ErrRelogin) || h.relog.Load() != 1 {
				t.Fatalf("check: %v", err)
			}
		})
	}
}

func TestPublishUploadFailure(t *testing.T) {
	h := newHarness(t, "FAKE_UPLOAD=fail")
	signedIn(t, h)
	_, err := h.w.Publish(context.Background(), item, provider.PublishRequest{Path: contentZip(t), Version: "1.7.1"}, nil)
	var pe *provider.PublishError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "Access Denied") || errors.Is(err, ErrRelogin) {
		t.Fatalf("upload failure: %v", err)
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
	ws := New(Options{DataDir: data, HTTP: srv.Client(), Download: srv.URL + "/client/installer/steamcmd.zip", Candidates: func() []string { return nil }})
	if st, _ := ws.Status(); st.SteamCMD != "" {
		t.Fatalf("found before install: %+v", st)
	}
	st, err := ws.Install(context.Background())
	if err != nil || st.Source != "app" || st.SteamCMD != filepath.Join(data, "tools", "steamcmd", "steamcmd.exe") {
		t.Fatalf("install: %+v %v", st, err)
	}
	if b, _ := os.ReadFile(st.SteamCMD); string(b) != "MZ steamcmd" {
		t.Fatalf("exe %q", b)
	}
}
