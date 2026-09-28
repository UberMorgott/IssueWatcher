package api

import (
	"encoding/json"
	"sync"
	"time"
)

// Opening the dashboard (tray, popup card, second instance). Every OpenBrowser
// call ends in exactly one logged decision:
//
//   - focus + navigate: a connected tab is steered over SSE (only the most
//     recently connected one, the active tab);
//   - new tab: no tab is connected, or its window cannot be brought to the
//     front (the old tabs get "superseded");
//   - queued: a tab this server just opened has not connected yet. Opening
//     another one would stack tabs, so the route waits and reaches that tab as
//     navigate once it connects; if it never connects within launchWait, the
//     route opens in a new tab.

// launchWait is how long a newly opened tab may take to connect its event stream.
const launchWait = 10 * time.Second

// opener serialises OpenBrowser and tracks the launched tab that has not
// connected yet. The zero value is ready to use.
type opener struct {
	mu      sync.Mutex
	pending *pendingLaunch
	gen     uint64
	wait    time.Duration // 0 = launchWait; tests shorten it
}

type pendingLaunch struct {
	gen    uint64
	timer  *time.Timer
	queued bool   // an OpenBrowser call arrived while the tab was connecting
	path   string // route for it ("" = just bring to front)
}

// OpenBrowser shows the dashboard at route next ("" = keep the current page).
// With Options.Focus (the desktop shell): when a tab is open and its browser
// window can be brought to the front, that tab is steered there over SSE;
// otherwise (background tab, other window) a new tab opens and the old tabs get
// "superseded", so a click never does visibly nothing. Without Focus an open tab
// is only steered over SSE.
func (s *Server) OpenBrowser(next string) {
	if next != "" {
		next = safeNext(next)
	}
	o := &s.opener
	o.mu.Lock()
	defer o.mu.Unlock()

	if p := o.pending; p != nil {
		if next != "" || !p.queued { // a later focus-only click keeps a queued route
			p.path = next
		}
		p.queued = true
		s.opts.Log.Info("api: new tab still connecting; route queued", "path", p.path)
		return
	}

	switch {
	case s.hub.count() == 0:
		s.opts.Log.Info("api: no dashboard tab connected; opening a new tab", "path", next)
	case s.opts.Focus == nil:
		if s.navigate(next) {
			return
		}
		s.opts.Log.Info("api: navigate reached no tab; opening a new tab", "path", next)
	default:
		if title, ok := s.opts.Focus(); ok {
			s.opts.Log.Info("api: dashboard window brought to front", "title", title)
			if s.navigate(next) {
				return
			}
			s.opts.Log.Info("api: focused tab not connected; opening a new tab", "path", next)
		} else {
			n := s.Publish(EventSuperseded, map[string]string{})
			s.opts.Log.Info("api: dashboard window not found by title; opening a new tab", "path", next, "superseded", n)
		}
	}
	s.launchLocked(next)
}

// StartGrace is how long a start waits for a tab of the previous run to
// reconnect before opening a new one. The SPA retries its event stream at
// least every 2.5 s while the app is down, so an open tab is back within it.
const StartGrace = 3 * time.Second

// OpenOnStart shows the dashboard at next when the app starts, unless a tab
// of the previous run reconnects within grace: after a restart the open tab
// comes back by itself (same port, persistent session), and a new tab then
// would be a duplicate. A reconnected tab is steered to next unless next is
// "/" (it keeps its page). Blocks up to grace.
func (s *Server) OpenOnStart(next string, grace time.Duration) {
	if s.hub.waitClient(grace) {
		s.opts.Log.Info("api: dashboard tab reconnected on start; not opening a new tab", "path", next)
		if next != "/" {
			s.navigate(next)
		}
		return
	}
	s.OpenBrowser(next)
}

// launchLocked opens a new tab at next and waits for it to connect. o.mu held.
func (s *Server) launchLocked(next string) {
	o := &s.opener
	o.gen++
	gen := o.gen
	wait := o.wait
	if wait <= 0 {
		wait = launchWait
	}
	o.pending = &pendingLaunch{gen: gen}
	o.pending.timer = time.AfterFunc(wait, func() { s.launchTimedOut(gen, wait) })
	s.opts.Open(s.LaunchURL(next))
}

// launchTimedOut ends a launch whose tab never connected; a route queued for it
// opens in a new tab so the click that queued it still shows something.
func (s *Server) launchTimedOut(gen uint64, wait time.Duration) {
	o := &s.opener
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.pending
	if p == nil || p.gen != gen {
		return // connected meanwhile, or superseded by a newer launch
	}
	o.pending = nil
	if !p.queued {
		s.opts.Log.Warn("api: new tab did not connect in time", "wait", wait)
		return
	}
	s.opts.Log.Warn("api: new tab did not connect in time; opening the queued route in a new tab", "wait", wait, "path", p.path)
	s.launchLocked(p.path)
}

// tabConnected ends a pending launch when a tab connects and hands it the route
// queued meanwhile.
func (s *Server) tabConnected(ch chan sseMsg) {
	o := &s.opener
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.pending
	if p == nil {
		return
	}
	o.pending = nil
	p.timer.Stop()
	if !p.queued {
		s.opts.Log.Info("api: new tab connected")
		return
	}
	b, _ := json.Marshal(map[string]string{"path": p.path}) // map[string]string never fails
	ok := s.hub.send(ch, sseMsg{event: EventNavigate, data: b})
	s.opts.Log.Info("api: new tab connected; queued route delivered", "path", p.path, "delivered", ok)
}
