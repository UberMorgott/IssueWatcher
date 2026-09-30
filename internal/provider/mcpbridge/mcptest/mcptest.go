// Package mcptest is a fake MCP server for provider tests: tools answer with
// canned `format:"json"` results over in-memory transports (no child process,
// no live site).
package mcptest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler answers one tool call: a result value (sent as structuredContent) or
// an error. A *CodeError becomes the servers' {error:{code,message}} result;
// ErrCrash drops the connection (a crashed server).
type Handler func(args map[string]any) (any, error)

// CodeError is a tool error result.
type CodeError struct{ Code, Message string }

func (e *CodeError) Error() string { return e.Code + ": " + e.Message }

// ErrCrash makes the fake drop the session instead of answering.
var ErrCrash = errors.New("mcptest: crash")

// Call is one recorded call.
type Call struct {
	Tool string
	Args map[string]any
}

// Server is the fake; its Dial starts a fresh session per (re)start.
type Server struct {
	mu     sync.Mutex
	tools  map[string]Handler
	calls  []Call
	starts int
}

// New creates an empty fake.
func New() *Server { return &Server{tools: map[string]Handler{}} }

// Handle sets tool's handler (replacing an earlier one).
func (s *Server) Handle(tool string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[tool] = h
}

// Calls returns the recorded calls.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Count returns how many calls tool got.
func (s *Server) Count(tool string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Tool == tool {
			n++
		}
	}
	return n
}

// Starts returns how many sessions were started.
func (s *Server) Starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts
}

// Dial is mcpbridge.Options.Dial.
func (s *Server) Dial(ctx context.Context) (mcp.Transport, error) {
	ct, st := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	s.mu.Lock()
	s.starts++
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	s.mu.Unlock()
	cur := &crashConn{}
	for _, name := range names {
		srv.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return s.answer(ctx, name, req, cur)
			})
	}
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		return nil, err
	}
	return &crashTransport{inner: ct, cur: cur}, nil
}

func (s *Server) answer(ctx context.Context, name string, req *mcp.CallToolRequest, cur *crashConn) (*mcp.CallToolResult, error) {
	args := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		_ = json.Unmarshal(req.Params.Arguments, &args)
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{Tool: name, Args: args})
	h := s.tools[name]
	s.mu.Unlock()
	v, err := h(args)
	if errors.Is(err, ErrCrash) {
		cur.crash()
		<-ctx.Done() // the session is closed under the call: never answer
		return nil, ctx.Err()
	}
	if err != nil {
		// Tool failures are results, not protocol errors (as the real servers answer).
		res := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
		if ce, ok := errors.AsType[*CodeError](err); ok {
			res.StructuredContent = map[string]any{"error": map[string]string{"code": ce.Code, "message": ce.Message}}
		}
		return res, nil
	}
	j, _ := json.Marshal(v)
	return &mcp.CallToolResult{StructuredContent: v, Content: []mcp.Content{&mcp.TextContent{Text: string(j)}}}, nil
}

// crashTransport lets ErrCrash break the client's side of the pipe, as a
// dying child process would.
type crashTransport struct {
	inner mcp.Transport
	cur   *crashConn
}

func (t *crashTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	t.cur.mu.Lock()
	t.cur.conn = c
	t.cur.mu.Unlock()
	return c, nil
}

type crashConn struct {
	mu   sync.Mutex
	conn mcp.Connection
}

func (c *crashConn) crash() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
