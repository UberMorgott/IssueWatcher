package browser

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"sync"

	"golang.org/x/net/websocket"
)

// MaxFrame bounds one CDP message (a widget page or a cookie list is far
// below it; a runaway answer drops the connection).
const MaxFrame = 64 << 20

// pipeTransport is --remote-debugging-pipe: NUL-terminated JSON messages over
// two pipes.
type pipeTransport struct {
	r   *bufio.Reader
	w   io.Writer
	max int

	closeOnce sync.Once
	closers   []io.Closer
}

// NewPipeTransport frames messages over r/w; closers run on Close.
func NewPipeTransport(r io.Reader, w io.Writer, maxFrame int, closers ...io.Closer) Transport {
	if maxFrame <= 0 {
		maxFrame = MaxFrame
	}
	return &pipeTransport{r: bufio.NewReaderSize(r, 64<<10), w: w, max: maxFrame, closers: closers}
}

func (p *pipeTransport) Read() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := p.r.ReadSlice(0)
		if len(buf)+len(chunk) > p.max+1 {
			return nil, ErrFrameTooLarge
		}
		switch {
		case err == nil:
			buf = append(buf, chunk[:len(chunk)-1]...)
			return buf, nil
		case errors.Is(err, bufio.ErrBufferFull):
			buf = append(buf, chunk...)
		default:
			return nil, err
		}
	}
}

func (p *pipeTransport) Write(msg []byte) error {
	if bytes.IndexByte(msg, 0) >= 0 {
		return errors.New("browser: NUL in a CDP message")
	}
	_, err := p.w.Write(append(msg, 0))
	return err
}

func (p *pipeTransport) Close() error {
	var err error
	p.closeOnce.Do(func() {
		for _, c := range p.closers {
			if cerr := c.Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
	})
	return err
}

// wsTransport is the fallback --remote-debugging-port endpoint.
type wsTransport struct {
	c *websocket.Conn
}

// DialWebSocket connects to a DevTools websocket URL (ws://127.0.0.1:<port>/…).
func DialWebSocket(url string) (Transport, error) {
	cfg, err := websocket.NewConfig(url, "http://127.0.0.1")
	if err != nil {
		return nil, err
	}
	c, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}
	c.MaxPayloadBytes = MaxFrame
	return &wsTransport{c: c}, nil
}

func (w *wsTransport) Read() ([]byte, error) {
	var b []byte
	if err := websocket.Message.Receive(w.c, &b); err != nil {
		if errors.Is(err, websocket.ErrFrameTooLarge) {
			return nil, ErrFrameTooLarge
		}
		return nil, err
	}
	return b, nil
}

func (w *wsTransport) Write(msg []byte) error { return websocket.Message.Send(w.c, string(msg)) }
func (w *wsTransport) Close() error           { return w.c.Close() }
