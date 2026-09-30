// Package browser drives the installed Chromium browser (Chrome, Edge, Cent,
// …) over the Chrome DevTools Protocol with its own profile under the data
// dir (docs/ARCHITECTURE.md → Native mod platforms): in-page fetches on sites
// that answer only a real browser, cookies for the sign-in capture, and a
// visible sign-in window. The CDP client is our own (flattened sessions,
// events, cancellation, crash, bounded frames); CDP payloads and cookies are
// never logged.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrClosed means the browser connection ended (crash, exit, Close): pending
// and later calls fail with it.
var ErrClosed = errors.New("browser: connection closed")

// ErrFrameTooLarge means the browser sent a message above the frame limit;
// the connection is dropped (a runaway page must not exhaust memory).
var ErrFrameTooLarge = errors.New("browser: CDP message too large")

// Transport carries whole CDP messages (one JSON object each).
type Transport interface {
	Read() ([]byte, error)
	Write(msg []byte) error
	Close() error
}

// CallError is a CDP error answer.
type CallError struct {
	Method  string `json:"-"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *CallError) Error() string {
	return fmt.Sprintf("browser: %s: %s (%d)", e.Method, e.Message, e.Code)
}

// Event is a CDP event; Session is "" for browser-level events.
type Event struct {
	Session string
	Method  string
	Params  json.RawMessage
}

type message struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *CallError      `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

type reply struct {
	result json.RawMessage
	err    error
}

// Conn is one CDP connection (browser endpoint; targets through flattened
// sessions). Safe for concurrent use.
type Conn struct {
	t Transport

	mu      sync.Mutex
	next    int64
	pending map[int64]chan reply
	subs    map[int]func(Event)
	subID   int
	err     error
	done    chan struct{}
	wsem    chan struct{} // one write at a time (a slot, so waiting honours ctx)
}

// NewConn starts reading t.
func NewConn(t Transport) *Conn {
	c := &Conn{t: t, pending: map[int64]chan reply{}, subs: map[int]func(Event){}, done: make(chan struct{}), wsem: make(chan struct{}, 1)}
	go c.read()
	return c
}

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended (nil while open).
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection; pending calls fail with ErrClosed.
func (c *Conn) Close() error {
	err := c.t.Close()
	c.fail(ErrClosed)
	return err
}

func (c *Conn) read() {
	for {
		b, err := c.t.Read()
		if err != nil {
			if errors.Is(err, ErrFrameTooLarge) {
				_ = c.t.Close()
				c.fail(err)
				return
			}
			c.fail(fmt.Errorf("%w: %w", ErrClosed, err))
			return
		}
		var m message
		if err := json.Unmarshal(b, &m); err != nil {
			continue // not ours to crash on; the browser never sends it
		}
		if m.ID != 0 {
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				r := reply{result: m.Result}
				if m.Error != nil {
					r.err = m.Error
				}
				ch <- r
			}
			continue
		}
		if m.Method == "" {
			continue
		}
		ev := Event{Session: m.SessionID, Method: m.Method, Params: m.Params}
		c.mu.Lock()
		fns := make([]func(Event), 0, len(c.subs))
		for _, fn := range c.subs {
			fns = append(fns, fn)
		}
		c.mu.Unlock()
		for _, fn := range fns {
			fn(ev)
		}
	}
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	for id, ch := range c.pending {
		ch <- reply{err: err}
		delete(c.pending, id)
	}
	close(c.done)
}

// On calls fn for every event (from the reader goroutine: fn must not block
// or call Call); the returned func unsubscribes.
func (c *Conn) On(fn func(Event)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subID++
	id := c.subID
	c.subs[id] = fn
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// Call sends method with params on session ("" = browser) and decodes the
// result into out (nil = ignore). ctx ends the wait (a late answer is dropped).
func (c *Conn) Call(ctx context.Context, session, method string, params, out any) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("browser: encode %s: %w", method, err)
		}
		raw = b
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	b, err := json.Marshal(message{ID: id, Method: method, Params: raw, SessionID: session})
	if err != nil {
		c.drop(id)
		return err
	}
	if err := c.write(ctx, b); err != nil {
		c.drop(id)
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			if ce, ok := errors.AsType[*CallError](r.err); ok {
				ce.Method = method
			}
			return r.err
		}
		if out != nil && len(r.result) > 0 {
			if err := json.Unmarshal(r.result, out); err != nil {
				return fmt.Errorf("browser: decode %s: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.drop(id)
		return ctx.Err()
	}
}

// write sends one message; ctx bounds both the wait for the writer slot and
// the write itself (a browser that stopped reading must not hang the caller).
// A write cut short by ctx finishes in the background (framing stays whole)
// and unblocks when the transport closes.
func (c *Conn) write(ctx context.Context, b []byte) error {
	select {
	case c.wsem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
	werr := make(chan error, 1)
	go func() {
		defer func() { <-c.wsem }()
		werr <- c.t.Write(b)
	}()
	select {
	case err := <-werr:
		if err != nil {
			return fmt.Errorf("%w: %w", ErrClosed, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Conn) drop(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}
