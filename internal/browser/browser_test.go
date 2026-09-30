package browser

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChrome answers the CDP calls Browser makes; fetch answers come from fetch.
type fakeChrome struct {
	mu      sync.Mutex
	title   string
	fetch   func(url string) Response
	cookies []Cookie
	starts  atomic.Int32
	args    [][]string
	uaSet   atomic.Int32
	crash   chan struct{} // closed = the current process dies
	// failAttach makes Target.attachToTarget fail; closed counts Target.closeTarget.
	failAttach atomic.Bool
	closed     atomic.Int32
	pages      []string // Target.getTargets: page target ids
}

func (f *fakeChrome) start(_ string, args []string) (*process, error) {
	f.starts.Add(1)
	f.mu.Lock()
	f.args = append(f.args, args)
	f.crash = make(chan struct{})
	crash := f.crash
	f.mu.Unlock()
	cr, pw := io.Pipe()
	pr, cw := io.Pipe()
	p := &process{t: NewPipeTransport(cr, cw, 0, cr, cw), kill: func() { _ = pw.Close(); _ = pr.Close() }, exited: make(chan struct{})}
	go func() {
		<-crash
		_ = pw.CloseWithError(errors.New("crashed"))
	}()
	go f.serve(pr, pw, p.exited)
	return p, nil
}

func (f *fakeChrome) serve(r io.Reader, w *io.PipeWriter, exited chan struct{}) {
	defer close(exited)
	peerT := NewPipeTransport(r, w, 0)
	var wmu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		_ = peerT.Write(b)
		wmu.Unlock()
	}
	for {
		b, err := peerT.Read()
		if err != nil {
			return
		}
		var m message
		_ = json.Unmarshal(b, &m)
		var result any = map[string]any{}
		switch m.Method {
		case "Browser.getVersion":
			result = map[string]string{"product": "HeadlessChrome/154.0.1.2", "userAgent": "Mozilla/5.0 HeadlessChrome/154.0.1.2 Safari/537.36"}
		case "Target.createTarget":
			result = map[string]string{"targetId": "T1"}
		case "Target.attachToTarget":
			if f.failAttach.Load() {
				send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "attach failed"}})
				continue
			}
			result = map[string]string{"sessionId": "S1"}
		case "Target.closeTarget":
			f.closed.Add(1)
		case "Emulation.setUserAgentOverride":
			var ua uaOverride
			_ = json.Unmarshal(m.Params, &ua)
			if !strings.Contains(ua.UserAgent, "HeadlessChrome") {
				f.uaSet.Add(1)
			}
		case "Page.navigate":
			send(map[string]any{"id": m.ID, "sessionId": m.SessionID, "result": map[string]string{"frameId": "F"}})
			send(map[string]any{"method": "Page.domContentEventFired", "sessionId": m.SessionID, "params": map[string]any{}})
			continue
		case "Runtime.evaluate":
			var p struct {
				Expression string `json:"expression"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.Expression == "document.title" {
				f.mu.Lock()
				title := f.title
				f.mu.Unlock()
				result = map[string]any{"result": map[string]any{"value": title}}
				break
			}
			i := strings.LastIndex(p.Expression, "})(")
			var a struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal([]byte(strings.TrimSuffix(p.Expression[i+3:], ")")), &a)
			f.mu.Lock()
			fetch := f.fetch
			f.mu.Unlock()
			result = map[string]any{"result": map[string]any{"value": fetch(a.URL)}}
		case "Target.getTargets":
			f.mu.Lock()
			infos := []map[string]string{}
			for _, id := range f.pages {
				infos = append(infos, map[string]string{"targetId": id, "type": "page", "url": "https://www.example.com/"})
			}
			f.mu.Unlock()
			result = map[string]any{"targetInfos": infos}
		case "Storage.getCookies":
			f.mu.Lock()
			result = map[string]any{"cookies": f.cookies}
			f.mu.Unlock()
		}
		send(map[string]any{"id": m.ID, "sessionId": m.SessionID, "result": result})
	}
}

func newFake(t *testing.T) (*fakeChrome, *Browser) {
	f := &fakeChrome{title: "Mod page", fetch: func(u string) Response { return Response{Status: 200, URL: u, Body: "ok"} }}
	b := New(Options{Dir: t.TempDir(), Origins: []string{"https://www.example.com"}, Start: f.start,
		Find: func() (Exe, error) { return Exe{Path: "chrome.exe", Name: "Fake"}, nil }, ChallengeWait: 50 * time.Millisecond})
	t.Cleanup(b.Close)
	return f, b
}

func TestFetchHeadlessWithUserAgentOverride(t *testing.T) {
	f, b := newFake(t)
	ctx := context.Background()
	r, err := b.Fetch(ctx, Request{URL: "https://www.example.com/api?x=1"})
	if err != nil || r.Status != 200 || r.Body != "ok" {
		t.Fatalf("fetch = %+v, %v", r, err)
	}
	if _, err := b.Fetch(ctx, Request{URL: "https://www.example.com/api?x=2"}); err != nil {
		t.Fatal(err)
	}
	if f.starts.Load() != 1 || f.uaSet.Load() != 1 {
		t.Fatalf("starts = %d, ua overrides = %d (want one browser, one tab)", f.starts.Load(), f.uaSet.Load())
	}
	args := strings.Join(f.args[0], " ")
	for _, want := range []string{"--headless=new", "--disable-blink-features=AutomationControlled", "--user-data-dir="} {
		if !strings.Contains(args, want) {
			t.Fatalf("args %q lack %s", args, want)
		}
	}
	if !strings.Contains(b.UserAgent(), "Chrome/154") || strings.Contains(b.UserAgent(), "Headless") {
		t.Fatalf("user agent = %q", b.UserAgent())
	}
}

func TestFetchOriginAllowlist(t *testing.T) {
	f, b := newFake(t)
	ctx := context.Background()
	for _, u := range []string{"http://www.example.com/", "https://evil.example.org/", "javascript:alert(1)"} {
		if _, err := b.Fetch(ctx, Request{URL: u}); !errors.Is(err, ErrOrigin) {
			t.Fatalf("%s: err = %v", u, err)
		}
	}
	if _, err := b.Fetch(ctx, Request{URL: "https://www.example.com/a", Page: "https://evil.example.org/"}); !errors.Is(err, ErrOrigin) {
		t.Fatalf("foreign page: err = %v", err)
	}
	f.setFetch(func(string) Response { return Response{Status: 200, URL: "https://evil.example.org/x", Body: "x"} })
	if _, err := b.Fetch(ctx, Request{URL: "https://www.example.com/r"}); !errors.Is(err, ErrOrigin) {
		t.Fatalf("redirect: err = %v", err)
	}
}

func TestFetchChallenge(t *testing.T) {
	f, b := newFake(t)
	f.mu.Lock()
	f.title = "Just a moment..."
	f.mu.Unlock()
	if _, err := b.Fetch(context.Background(), Request{URL: "https://www.example.com/a"}); !errors.Is(err, ErrChallenge) {
		t.Fatalf("challenged page: err = %v", err)
	}
	f.mu.Lock()
	f.title = "ok"
	f.mu.Unlock()
	f.setFetch(func(u string) Response {
		return Response{Status: 403, URL: u, Body: "<title>Just a moment...</title>"}
	})
	if _, err := b.Fetch(context.Background(), Request{URL: "https://www.example.com/a"}); !errors.Is(err, ErrChallenge) {
		t.Fatalf("challenged fetch: err = %v", err)
	}
}

func TestBrowserRestartsAfterCrash(t *testing.T) {
	f, b := newFake(t)
	ctx := context.Background()
	if _, err := b.Fetch(ctx, Request{URL: "https://www.example.com/a"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	close(f.crash)
	f.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if _, err := b.Fetch(ctx, Request{URL: "https://www.example.com/b"}); err != nil {
		t.Fatalf("after crash: %v", err)
	}
	if f.starts.Load() != 2 {
		t.Fatalf("starts = %d", f.starts.Load())
	}
}

func TestCookiesFilteredByDomain(t *testing.T) {
	f, b := newFake(t)
	f.cookies = []Cookie{
		{Name: "a", Value: "1", Domain: ".example.com"},
		{Name: "b", Value: "2", Domain: "www.example.com"},
		{Name: "c", Value: "3", Domain: "notexample.com"},
		{Name: "d", Value: "4", Domain: ".other.org"},
	}
	cks, err := b.Cookies(context.Background(), []string{"example.com"})
	if err != nil || len(cks) != 2 || cks[0].Name != "a" || cks[1].Name != "b" {
		t.Fatalf("cookies = %+v, %v", cks, err)
	}
}

func TestIdleStop(t *testing.T) {
	f := &fakeChrome{title: "x", fetch: func(u string) Response { return Response{Status: 200, URL: u} }}
	b := New(Options{Dir: t.TempDir(), Origins: []string{"https://www.example.com"}, Start: f.start, IdleStop: 30 * time.Millisecond,
		Find: func() (Exe, error) { return Exe{Path: "chrome.exe"}, nil }})
	defer b.Close()
	if _, err := b.Fetch(context.Background(), Request{URL: "https://www.example.com/a"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for b.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Running() {
		t.Fatal("browser still running after the idle stop")
	}
}

func TestStopDoesNotHangOnStuckPipe(t *testing.T) {
	b := New(Options{Dir: t.TempDir()})
	b.stopWait = 100 * time.Millisecond
	st := newStuck()
	var killed atomic.Bool
	p := &process{t: st, kill: func() { killed.Store(true); _ = st.Close() }, exited: make(chan struct{})}
	b.proc, b.conn = p, NewConn(st)
	done := make(chan struct{})
	go func() { b.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung on Browser.close over a stuck pipe")
	}
	if !killed.Load() {
		t.Fatal("process not killed")
	}
}

func TestCloseDuringStartKillsLaunch(t *testing.T) {
	f := &fakeChrome{title: "x", fetch: func(u string) Response { return Response{Status: 200, URL: u} }}
	entered, release := make(chan struct{}), make(chan struct{})
	var killed atomic.Bool
	b := New(Options{Dir: t.TempDir(), Origins: []string{"https://www.example.com"},
		Find: func() (Exe, error) { return Exe{Path: "chrome.exe"}, nil },
		Start: func(exe string, args []string) (*process, error) {
			close(entered)
			<-release
			p, err := f.start(exe, args)
			if err == nil {
				kill := p.kill
				p.kill = func() { killed.Store(true); kill() }
			}
			return p, err
		}})
	b.stopWait = 100 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := b.Fetch(context.Background(), Request{URL: "https://www.example.com/a"})
		done <- err
	}()
	<-entered
	b.Close()
	close(release)
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("fetch after Close err = %v", err)
	}
	if b.Running() || !killed.Load() {
		t.Fatalf("running = %v, killed = %v: the launch outlived Close", b.Running(), killed.Load())
	}
}

func TestDroppedTabsAreClosed(t *testing.T) {
	f, b := newFake(t)
	ctx := context.Background()
	u := "https://www.example.com/a"
	f.setFetch(func(u string) Response { return Response{Status: 403, URL: u, Body: "<title>Just a moment...</title>"} })
	if _, err := b.Fetch(ctx, Request{URL: u}); !errors.Is(err, ErrChallenge) {
		t.Fatalf("challenged fetch: err = %v", err)
	}
	if n := f.closed.Load(); n != 1 {
		t.Fatalf("after challenge: closeTarget = %d, want 1", n)
	}
	f.setFetch(func(u string) Response { return Response{Status: 200, URL: u} })
	if _, err := b.Fetch(ctx, Request{URL: u}); err != nil {
		t.Fatal(err)
	}
	if err := b.ClearSite(ctx, []string{"example.com"}, []string{"https://www.example.com"}); err != nil {
		t.Fatal(err)
	}
	if n := f.closed.Load(); n != 2 {
		t.Fatalf("after ClearSite: closeTarget = %d, want 2", n)
	}
	f.failAttach.Store(true)
	if _, err := b.Fetch(ctx, Request{URL: u}); err == nil {
		t.Fatal("fetch with a failing attach succeeded")
	}
	if n := f.closed.Load(); n != 3 {
		t.Fatalf("after a failed attach: closeTarget = %d, want 3 (the half-made target)", n)
	}
}

// TestLaunchWaitsForOutsideStop: a sign-in's Stop (outside b.op) must finish
// before a sync's launch on the same profile, else Chromium hands the launch
// to the exiting instance ("pipe: EOF; port: … exited before DevTools came up").
func TestLaunchWaitsForOutsideStop(t *testing.T) {
	f := &fakeChrome{title: "x", fetch: func(u string) Response { return Response{Status: 200, URL: u} }}
	var alive, overlap atomic.Int32
	b := New(Options{Dir: t.TempDir(), Origins: []string{"https://www.example.com"},
		Find: func() (Exe, error) { return Exe{Path: "chrome.exe"}, nil },
		Start: func(exe string, args []string) (*process, error) {
			if alive.Load() > 0 {
				overlap.Add(1)
			}
			p, err := f.start(exe, args)
			if err != nil {
				return nil, err
			}
			alive.Add(1)
			exited, slow := p.exited, make(chan struct{})
			go func() { // the real browser takes a while to exit after Browser.close
				<-exited
				time.Sleep(150 * time.Millisecond)
				alive.Add(-1)
				close(slow)
			}()
			p.exited = slow
			return p, nil
		}})
	t.Cleanup(b.Close)
	ctx := context.Background()
	if _, err := b.Fetch(ctx, Request{URL: "https://www.example.com/a"}); err != nil {
		t.Fatal(err)
	}
	go b.Stop()
	for b.Running() {
		time.Sleep(time.Millisecond)
	}
	if _, err := b.Fetch(ctx, Request{URL: "https://www.example.com/b"}); err != nil {
		t.Fatalf("fetch after a stop: %v", err)
	}
	if n := overlap.Load(); n != 0 {
		t.Fatalf("%d launch(es) while the previous browser still ran on the profile", n)
	}
}

// TestWindowOpenIgnoresReadTabs: a sync's read tab in the sign-in browser
// does not keep the sign-in "open" once the user closed the sign-in tab.
func TestWindowOpenIgnoresReadTabs(t *testing.T) {
	f, b := newFake(t)
	ctx := context.Background()
	f.mu.Lock()
	f.pages = []string{"LOGIN"}
	f.mu.Unlock()
	if err := b.OpenWindow(ctx, "https://www.example.com/login"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Fetch(ctx, Request{URL: "https://www.example.com/a"}); err != nil { // reuses the headed browser (tab T1)
		t.Fatal(err)
	}
	if f.starts.Load() != 1 {
		t.Fatalf("starts = %d: the read did not reuse the sign-in browser", f.starts.Load())
	}
	f.mu.Lock()
	f.pages = []string{"LOGIN", "T1"}
	f.mu.Unlock()
	if !b.WindowOpen(ctx) {
		t.Fatal("sign-in tab open: WindowOpen = false")
	}
	f.mu.Lock()
	f.pages = []string{"T1"}
	f.mu.Unlock()
	if b.WindowOpen(ctx) {
		t.Fatal("only the read tab left: WindowOpen = true")
	}
}

func (f *fakeChrome) setFetch(fn func(string) Response) {
	f.mu.Lock()
	f.fetch = fn
	f.mu.Unlock()
}
