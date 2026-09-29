package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// Live events for open dashboard tabs (GET /api/events, Server-Sent Events).
//
// Event names: item.new, comment.new, item.closed, sync.status, data.changed,
// auth.changed, navigate {path}, superseded. The hub also tells the tray whether a tab is
// open, so a tray click can steer that tab (navigate) instead of opening a new one.

// SSE event names.
const (
	EventItemNew     = "item.new"
	EventCommentNew  = "comment.new"
	EventItemClosed  = "item.closed"
	EventSyncStatus  = "sync.status"  // syncer.Progress
	EventDataChanged = "data.changed" // DataChange: pages refetch what they show
	EventAuthChanged = "auth.changed"
	EventNavigate    = "navigate"
	EventSuperseded  = "superseded" // a new dashboard tab replaces the open ones
)

// DataChange is the data.changed payload: why the stored data changed and, for
// local actions, which item.
type DataChange struct {
	Reason string `json:"reason"` // sync | read | reply | job | folder | labels
	ItemID int64  `json:"itemId,omitempty"`
	Repo   string `json:"repo,omitempty"`
}

// Pacing of sync events per source: a cycle over many projects must not flood
// the tabs (a 32-slot buffer drops a slow tab, whose reconnect reloads
// everything). sync.status progress goes out at most every syncStatusEvery
// (the latest step wins), started/done/error at once; the data.changed of
// the steps that wrote something coalesce into one per dataChangedEvery.
const (
	syncStatusEvery  = 250 * time.Millisecond
	dataChangedEvery = 500 * time.Millisecond
)

// syncPacer holds each source's pending sync events.
type syncPacer struct {
	mu  sync.Mutex
	src map[string]*sourcePace
}

type sourcePace struct {
	lastStatus time.Time
	pending    *syncer.Progress // progress held back by the pace
	statusT    *time.Timer
	dirty      bool   // a step changed data since the last data.changed
	repo       string // the one repo changed in the window, " = several
	repos      int
	dataT      *time.Timer
}

// syncProgress mirrors a sync step to the open tabs (paced, see syncPacer):
// sync.status, plus data.changed when steps wrote something (the first,
// silent sync too).
func (s *Server) syncProgress(p syncer.Progress) {
	s.pace.mu.Lock()
	if s.pace.src == nil {
		s.pace.src = map[string]*sourcePace{}
	}
	sp := s.pace.src[p.Source]
	if sp == nil {
		sp = &sourcePace{}
		s.pace.src[p.Source] = sp
	}
	final := p.State == syncer.ProgressDone || p.State == syncer.ProgressError
	if p.Changed > 0 && !final {
		if sp.repos == 0 || sp.repo != p.Repo {
			sp.repos++
		}
		sp.repo, sp.dirty = p.Repo, true
		if sp.dataT == nil {
			sp.dataT = time.AfterFunc(dataChangedEvery, func() { s.flushData(p.Source) })
		}
	}
	var out []sseEvent
	now := time.Now()
	switch {
	case final:
		sp.pending = nil
		if sp.statusT != nil {
			sp.statusT.Stop()
			sp.statusT = nil
		}
		out = append(out, s.takeDataLocked(sp)...) // the data first: done refreshes what the tab shows
		out = append(out, sseEvent{EventSyncStatus, p})
		sp.lastStatus = now
	case p.State == syncer.ProgressStarted || now.Sub(sp.lastStatus) >= syncStatusEvery:
		sp.pending = nil
		out = append(out, sseEvent{EventSyncStatus, p})
		sp.lastStatus = now
	default:
		sp.pending = &p
		if sp.statusT == nil {
			sp.statusT = time.AfterFunc(syncStatusEvery-now.Sub(sp.lastStatus), func() { s.flushStatus(p.Source) })
		}
	}
	s.pace.mu.Unlock()
	for _, e := range out {
		s.Publish(e.name, e.data)
	}
}

type sseEvent struct {
	name string
	data any
}

// takeDataLocked returns the coalesced data.changed of sp, if any. s.pace.mu held.
func (s *Server) takeDataLocked(sp *sourcePace) []sseEvent {
	if sp.dataT != nil {
		sp.dataT.Stop()
		sp.dataT = nil
	}
	if !sp.dirty {
		return nil
	}
	repo := sp.repo
	if sp.repos > 1 {
		repo = ""
	}
	sp.dirty, sp.repo, sp.repos = false, "", 0
	return []sseEvent{{EventDataChanged, DataChange{Reason: "sync", Repo: repo}}}
}

func (s *Server) flushData(source string) {
	s.pace.mu.Lock()
	var out []sseEvent
	if sp := s.pace.src[source]; sp != nil {
		sp.dataT = nil
		out = s.takeDataLocked(sp)
	}
	s.pace.mu.Unlock()
	for _, e := range out {
		s.Publish(e.name, e.data)
	}
}

func (s *Server) flushStatus(source string) {
	s.pace.mu.Lock()
	var p *syncer.Progress
	if sp := s.pace.src[source]; sp != nil {
		p, sp.pending, sp.statusT = sp.pending, nil, nil
		if p != nil {
			sp.lastStatus = time.Now()
		}
	}
	s.pace.mu.Unlock()
	if p != nil {
		s.Publish(EventSyncStatus, *p)
	}
}

// dataChanged tells the open tabs that a local action changed stored data.
func (s *Server) dataChanged(reason string, itemID int64) {
	s.Publish(EventDataChanged, DataChange{Reason: reason, ItemID: itemID})
}

const (
	sseHeartbeat = 25 * time.Second
	sseBuffer    = 32 // per client; a client that falls this far behind is dropped
)

type sseMsg struct {
	event string
	data  []byte
}

// hub fans events out to connected SSE clients.
type hub struct {
	mu      sync.Mutex
	seq     uint64
	clients map[chan sseMsg]uint64 // → connect order; the highest is the active tab
	joined  chan struct{}          // signalled (non-blocking) on every subscribe
}

func newHub() *hub { return &hub{clients: map[chan sseMsg]uint64{}, joined: make(chan struct{}, 1)} }

// waitClient reports whether a client is connected now or connects within d.
func (h *hub) waitClient(d time.Duration) bool {
	if h.count() > 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-h.joined:
			if h.count() > 0 {
				return true
			}
		case <-t.C:
			return h.count() > 0
		}
	}
}

func (h *hub) subscribe() chan sseMsg {
	ch := make(chan sseMsg, sseBuffer)
	h.mu.Lock()
	h.seq++
	h.clients[ch] = h.seq
	h.mu.Unlock()
	select {
	case h.joined <- struct{}{}:
	default:
	}
	return ch
}

func (h *hub) unsubscribe(ch chan sseMsg) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// closeAll ends every stream (server shutdown: SSE handlers never return on their own).
func (h *hub) closeAll() {
	h.mu.Lock()
	for ch := range h.clients {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// publish sends to every client and returns how many got it. A client whose
// buffer is full is disconnected (its EventSource reconnects and reloads).
func (h *hub) publish(m sseMsg) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for ch := range h.clients {
		if h.sendLocked(ch, m) {
			n++
		}
	}
	return n
}

// publishLatest sends to the most recently connected client only: the active
// tab (a newer tab supersedes the older ones, and a taken-over tab reconnects).
func (h *hub) publishLatest(m sseMsg) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	var latest chan sseMsg
	var top uint64
	for ch, seq := range h.clients {
		if seq > top {
			latest, top = ch, seq
		}
	}
	return latest != nil && h.sendLocked(latest, m)
}

// send delivers to one client and reports whether it got the message.
func (h *hub) send(ch chan sseMsg, m sseMsg) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[ch]; !ok {
		return false
	}
	return h.sendLocked(ch, m)
}

// sendLocked queues m for ch; a full buffer disconnects the client. h.mu held.
func (h *hub) sendLocked(ch chan sseMsg, m sseMsg) bool {
	select {
	case ch <- m:
		return true
	default:
		delete(h.clients, ch)
		close(ch)
		return false
	}
}

// Publish sends a live event to all open dashboard tabs; it returns the number
// of tabs reached.
func (s *Server) Publish(event string, data any) int {
	b, err := json.Marshal(data)
	if err != nil {
		s.opts.Log.Error("api: event encode", "event", event, "err", err)
		return 0
	}
	return s.hub.publish(sseMsg{event: event, data: b})
}

// Clients is the number of connected dashboard tabs.
func (s *Server) Clients() int { return s.hub.count() }

// navigate steers the active tab (the most recently connected one, never a
// superseded tab) to path ("" = just bring to front) and reports whether it
// received the event.
func (s *Server) navigate(path string) bool {
	if path != "" {
		path = safeNext(path)
	}
	b, _ := json.Marshal(map[string]string{"path": path}) // map[string]string never fails
	ok := s.hub.publishLatest(sseMsg{event: EventNavigate, data: b})
	if ok {
		s.opts.Log.Info("api: navigate sent to the active tab", "path", path, "clients", s.hub.count())
	}
	return ok
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Reconnect delay hint for EventSource; the page adds its own backoff.
	if _, err := fmt.Fprint(w, "retry: 2000\n: connected\n\n"); err != nil || rc.Flush() != nil {
		return
	}

	ch := s.hub.subscribe()
	s.opts.Log.Info("api: events client connected", "clients", s.hub.count())
	defer func() {
		s.hub.unsubscribe(ch)
		s.opts.Log.Info("api: events client gone", "clients", s.hub.count()) // count after leaving
	}()
	s.tabConnected(ch) // a tab this server opened: deliver the route queued for it

	tick := time.NewTicker(sseHeartbeat)
	defer tick.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_, err = fmt.Fprint(w, ": ping\n\n")
		case m, ok := <-ch:
			if !ok {
				return // dropped as too slow
			}
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.event, m.data)
		}
		if err != nil || rc.Flush() != nil {
			return
		}
	}
}
