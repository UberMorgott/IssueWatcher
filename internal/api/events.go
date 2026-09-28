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
// auth.changed, navigate {path}. The hub also tells the tray whether a tab is
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
)

// DataChange is the data.changed payload: why the stored data changed and, for
// local actions, which item.
type DataChange struct {
	Reason string `json:"reason"` // sync | read | reply
	ItemID int64  `json:"itemId,omitempty"`
	Repo   string `json:"repo,omitempty"`
}

// syncProgress mirrors a sync step to the open tabs: always sync.status, plus
// data.changed when the step wrote something (the first, silent sync too).
func (s *Server) syncProgress(p syncer.Progress) {
	s.Publish(EventSyncStatus, p)
	if p.Changed > 0 && p.State != syncer.ProgressDone && p.State != syncer.ProgressError {
		s.Publish(EventDataChanged, DataChange{Reason: "sync", Repo: p.Repo})
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
	clients map[chan sseMsg]struct{}
}

func newHub() *hub { return &hub{clients: map[chan sseMsg]struct{}{}} }

func (h *hub) subscribe() chan sseMsg {
	ch := make(chan sseMsg, sseBuffer)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
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
		select {
		case ch <- m:
			n++
		default:
			delete(h.clients, ch)
			close(ch)
		}
	}
	return n
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

// navigate steers the open tabs to path ("" = just bring to front) and
// reports whether any tab received it.
func (s *Server) navigate(path string) bool {
	if path != "" {
		path = safeNext(path)
	}
	n := s.Publish(EventNavigate, map[string]string{"path": path})
	if n > 0 {
		s.opts.Log.Info("api: navigate sent", "path", path, "clients", n)
	}
	return n > 0
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
	defer s.hub.unsubscribe(ch)
	s.opts.Log.Info("api: events client connected", "clients", s.hub.count())
	defer func() { s.opts.Log.Info("api: events client gone", "clients", s.hub.count()) }()

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
