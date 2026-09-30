package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// peer is a fake browser on the far end of a pipe transport.
type peer struct {
	t      *testing.T
	in     *io.PipeReader // what the client writes
	out    *io.PipeWriter // what the client reads
	client Transport
}

func newPeer(t *testing.T, maxFrame int) *peer {
	cr, pw := io.Pipe() // client reads cr, peer writes pw
	pr, cw := io.Pipe() // peer reads pr, client writes cw
	p := &peer{t: t, in: pr, out: pw}
	p.client = NewPipeTransport(cr, cw, maxFrame, cr, cw)
	return p
}

// next reads one client message.
func (p *peer) next() message {
	p.t.Helper()
	var buf []byte
	one := make([]byte, 1)
	for {
		if _, err := p.in.Read(one); err != nil {
			p.t.Fatalf("peer read: %v", err)
		}
		if one[0] == 0 {
			break
		}
		buf = append(buf, one[0])
	}
	var m message
	if err := json.Unmarshal(buf, &m); err != nil {
		p.t.Fatalf("peer decode %q: %v", buf, err)
	}
	return m
}

func (p *peer) send(v any) {
	b, _ := json.Marshal(v)
	_, _ = p.out.Write(append(b, 0))
}

func TestConnInterleavedEventsAndReplies(t *testing.T) {
	p := newPeer(t, 0)
	c := NewConn(p.client)
	defer func() { _ = c.Close() }()
	var mu sync.Mutex
	var events []string
	off := c.On(func(ev Event) {
		mu.Lock()
		events = append(events, ev.Session+":"+ev.Method)
		mu.Unlock()
	})
	defer off()
	type res struct {
		v   string
		err error
	}
	results := make(chan res, 2)
	for _, m := range []string{"A.one", "B.two"} {
		go func() {
			var out struct {
				V string `json:"v"`
			}
			err := c.Call(context.Background(), "S1", m, map[string]int{"x": 1}, &out)
			results <- res{out.V, err}
		}()
	}
	m1, m2 := p.next(), p.next()
	if m1.SessionID != "S1" || m2.SessionID != "S1" {
		t.Fatalf("session not routed: %+v %+v", m1, m2)
	}
	// Events around and between the answers, answers out of order.
	p.send(map[string]any{"method": "Page.loadEventFired", "sessionId": "S1", "params": map[string]any{}})
	p.send(map[string]any{"id": m2.ID, "result": map[string]string{"v": m2.Method}})
	p.send(map[string]any{"method": "Target.detachedFromTarget", "params": map[string]string{"sessionId": "S1"}})
	p.send(map[string]any{"id": m1.ID, "result": map[string]string{"v": m1.Method}})
	got := map[string]bool{}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		got[r.v] = true
	}
	if !got["A.one"] || !got["B.two"] {
		t.Fatalf("results = %v", got)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(events, ",") != "S1:Page.loadEventFired,:Target.detachedFromTarget" {
		t.Fatalf("events = %v", events)
	}
}

func TestConnErrorAnswer(t *testing.T) {
	p := newPeer(t, 0)
	c := NewConn(p.client)
	defer func() { _ = c.Close() }()
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "", "Target.attachToTarget", nil, nil) }()
	m := p.next()
	p.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "No target with given id found"}})
	err := <-done
	ce, ok := errors.AsType[*CallError](err)
	if !ok || ce.Method != "Target.attachToTarget" || ce.Code != -32000 {
		t.Fatalf("err = %v", err)
	}
}

func TestConnCrashFailsPendingCalls(t *testing.T) {
	p := newPeer(t, 0)
	c := NewConn(p.client)
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "", "Browser.getVersion", nil, nil) }()
	p.next()
	_ = p.out.CloseWithError(errors.New("browser crashed"))
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call err = %v", err)
	}
	<-c.Done()
	if err := c.Call(context.Background(), "", "Browser.getVersion", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after crash err = %v", err)
	}
}

func TestConnCancelDropsLateAnswer(t *testing.T) {
	p := newPeer(t, 0)
	c := NewConn(p.client)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, "", "Runtime.evaluate", nil, nil) }()
	m := p.next()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call err = %v", err)
	}
	p.send(map[string]any{"id": m.ID, "result": map[string]any{}}) // late: dropped
	go func() { done <- c.Call(context.Background(), "", "Browser.getVersion", nil, nil) }()
	m2 := p.next()
	p.send(map[string]any{"id": m2.ID, "result": map[string]any{}})
	if err := <-done; err != nil {
		t.Fatalf("next call err = %v", err)
	}
}

func TestConnOversizeFrameDropsConnection(t *testing.T) {
	p := newPeer(t, 1024)
	c := NewConn(p.client)
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "", "Runtime.evaluate", nil, nil) }()
	m := p.next()
	big := bytes.Repeat([]byte("x"), 4096)
	go p.send(map[string]any{"id": m.ID, "result": map[string]string{"v": string(big)}})
	if err := <-done; !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestPipeTransportFramesLargeMessages(t *testing.T) {
	p := newPeer(t, 0)
	c := NewConn(p.client)
	defer func() { _ = c.Close() }()
	done := make(chan error, 1)
	var out struct {
		V string `json:"v"`
	}
	go func() { done <- c.Call(context.Background(), "", "Runtime.evaluate", nil, &out) }()
	m := p.next()
	big := strings.Repeat("y", 300<<10) // above the 64 KiB read buffer
	go p.send(map[string]any{"id": m.ID, "result": map[string]string{"v": big}})
	if err := <-done; err != nil || out.V != big {
		t.Fatalf("err = %v, len = %d", err, len(out.V))
	}
}
