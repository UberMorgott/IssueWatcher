// Package mcpbridge runs one of the owner's MCP servers (Nexus Mods,
// CurseForge) as a long-lived child and calls its tools with `format:"json"`
// (docs/ARCHITECTURE.md → Mod platforms → Bridge).
//
// One child per Client, started lazily inside a kill-on-close Job Object and
// stopped after IdleStop without calls; calls are serialized (one headless
// browser per server), each bounded by CallTimeout. A broken connection is
// restarted once; reads are then retried, writes never (the answer may be
// lost after the server acted: ErrOutcomeUnknown, the caller reads back).
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool error codes of the servers' JSON mode.
const (
	CodeNotLoggedIn = "not_logged_in"
	CodeCloudflare  = "cloudflare"
	CodeNotFound    = "not_found"
	CodeDisabled    = "disabled"
	CodeRateLimited = "rate_limited"
	CodeInvalid     = "invalid"
	// CodeOutcomeUnknown: a write request was sent and failed without a
	// refusal (timeout, 5xx, odd answer): it may have been saved.
	CodeOutcomeUnknown = "outcome_unknown"
	CodeError          = "error" // unclassified
)

// ErrUnavailable means the server cannot be started (not configured, missing
// file, failed to launch).
var ErrUnavailable = errors.New("mcp server unavailable")

// ErrOutcomeUnknown means a write was sent but its answer was lost (timeout,
// crash): it may or may not have happened.
var ErrOutcomeUnknown = errors.New("outcome unknown")

// ToolError is a tool's own error result {error:{code,message}}.
type ToolError struct {
	Tool    string
	Code    string
	Message string
}

func (e *ToolError) Error() string { return e.Tool + ": " + e.Code + ": " + e.Message }

// IsCode reports whether err is a ToolError with code.
func IsCode(err error, code string) bool {
	te, ok := errors.AsType[*ToolError](err)
	return ok && te.Code == code
}

// Options configures a Client.
type Options struct {
	Name string // platform, for logs and errors
	// Command returns the server command line (read at every start, so a
	// settings change applies on the next start).
	Command func() (string, []string)
	// Dial replaces Command (tests: in-memory transports).
	Dial        func(ctx context.Context) (mcp.Transport, error)
	CallTimeout time.Duration // default 60 s
	IdleStop    time.Duration // default 10 min
	Log         *slog.Logger
}

// Client is one MCP server child.
type Client struct {
	opts Options

	calls chan struct{} // one call at a time (one headless browser); waiting honours ctx

	mu     sync.Mutex // guards the fields below; never held across a tool call
	sess   *mcp.ClientSession
	kill   func() // ends the child's process tree (nil for Dial)
	idle   *time.Timer
	gen    int  // bumped on every stop and call start: a stale idle timer does nothing
	closed bool // Close was called: no new child is started any more
}

// New creates a Client; nothing starts until the first call.
func New(opts Options) *Client {
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 60 * time.Second
	}
	if opts.IdleStop <= 0 {
		opts.IdleStop = 10 * time.Minute
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return &Client{opts: opts, calls: make(chan struct{}, 1)}
}

// Call runs tool with args plus format:"json" and decodes its structured
// result into out (nil = ignore). read marks a read-only tool: after a broken
// connection it is retried once on a fresh child; a write is not and returns
// ErrOutcomeUnknown instead. Calls are serialized; waiting for the previous
// one ends with ctx.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any, out any, read bool) error {
	select {
	case c.calls <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.calls }()
	a := map[string]any{"format": "json"}
	maps.Copy(a, args)
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		c.gen++ // a pending idle stop must not end this call's child
		if c.idle != nil {
			c.idle.Stop()
			c.idle = nil
		}
		sess, err := c.startLocked(ctx)
		c.mu.Unlock()
		if err != nil {
			return err
		}
		res, err := c.callTool(ctx, sess, tool, a)
		c.mu.Lock()
		if err == nil {
			c.touchLocked()
			c.mu.Unlock()
			return decode(tool, res, out)
		}
		// Transport failure (crash, timeout, closed pipe, Stop/Close meanwhile): the child is not trusted any more.
		if c.sess == sess {
			c.stopLocked()
		}
		closed := c.closed
		c.mu.Unlock()
		if closed && read {
			return fmt.Errorf("%w: %s: closed", ErrUnavailable, c.opts.Name)
		}
		if ctx.Err() != nil {
			if read {
				return ctx.Err()
			}
			return fmt.Errorf("%s %s: %w: %w", c.opts.Name, tool, ErrOutcomeUnknown, ctx.Err())
		}
		if !read {
			return fmt.Errorf("%s %s: %w: %w", c.opts.Name, tool, ErrOutcomeUnknown, err)
		}
		if attempt >= 1 {
			return fmt.Errorf("%s %s: %w", c.opts.Name, tool, err)
		}
		c.opts.Log.Warn("mcp: restarting server", "server", c.opts.Name, "tool", tool, "err", err)
	}
}

func (c *Client) callTool(ctx context.Context, sess *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	cctx, cancel := context.WithTimeout(ctx, c.opts.CallTimeout)
	defer cancel()
	return sess.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
}

// Stop ends the child; the next call starts a new one (a changed command).
// An in-flight call fails at once (a write: ErrOutcomeUnknown).
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

// Close ends the child for good (platform switched off, app exit): later
// calls fail with ErrUnavailable instead of starting an unowned server.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.stopLocked()
}

// Running reports whether a child is up (tests).
func (c *Client) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess != nil
}

func (c *Client) startLocked(ctx context.Context) (*mcp.ClientSession, error) {
	if c.sess != nil {
		return c.sess, nil
	}
	if c.closed {
		return nil, fmt.Errorf("%w: %s: closed", ErrUnavailable, c.opts.Name)
	}
	var (
		t    mcp.Transport
		kill func()
		err  error
	)
	if c.opts.Dial != nil {
		t, err = c.opts.Dial(ctx)
	} else {
		t, kill, err = c.commandTransport()
	}
	if err != nil {
		return nil, err
	}
	cl := mcp.NewClient(&mcp.Implementation{Name: "issuewatcher", Version: "1"}, nil)
	sctx, cancel := context.WithTimeout(ctx, c.opts.CallTimeout)
	defer cancel()
	sess, err := cl.Connect(sctx, t, nil)
	if err != nil {
		if kill != nil {
			kill()
		}
		return nil, fmt.Errorf("%w: %s: start: %w", ErrUnavailable, c.opts.Name, err)
	}
	c.sess, c.kill = sess, kill
	c.opts.Log.Info("mcp: server started", "server", c.opts.Name)
	return sess, nil
}

func (c *Client) commandTransport() (mcp.Transport, func(), error) {
	if c.opts.Command == nil {
		return nil, nil, fmt.Errorf("%w: %s: no server command", ErrUnavailable, c.opts.Name)
	}
	name, args := c.opts.Command()
	if strings.TrimSpace(name) == "" {
		return nil, nil, fmt.Errorf("%w: %s: no server command", ErrUnavailable, c.opts.Name)
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, c.opts.Name, err)
	}
	for _, a := range args { // a missing script fails fast instead of a node stack trace
		if ext := strings.ToLower(filepath.Ext(a)); (ext == ".js" || ext == ".mjs") && filepath.IsAbs(a) {
			if _, err := os.Stat(a); err != nil {
				return nil, nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, c.opts.Name, err)
			}
		}
	}
	// Full parent environment (exec default): the servers' browsers fail with a
	// restricted one. The child starts suspended and joins a kill-on-close job.
	cmd := exec.Command(path, args...) //nolint:gosec,noctx // G204: the user's configured server; lifetime = the session, not a request
	prepareChild(cmd)
	cmd.Stderr = &stderrLog{log: c.opts.Log, server: c.opts.Name}
	j := &jobTransport{inner: &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}}
	return j, j.kill, nil
}

// jobTransport starts the command suspended, puts it in a job and resumes it.
type jobTransport struct {
	inner *mcp.CommandTransport
	tree  *procTree
}

func (t *jobTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	tree, err := adopt(t.inner.Command)
	if err != nil {
		_ = t.inner.Command.Process.Kill()
		_ = conn.Close()
		return nil, err
	}
	t.tree = tree
	return conn, nil
}

func (t *jobTransport) kill() { t.tree.kill() }

func (c *Client) stopLocked() {
	c.gen++
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	if c.sess == nil {
		return
	}
	// Closing waits for the child (up to TerminateDuration) and for an in-flight
	// call: done in the background so no caller blocks on it (at app exit the
	// kill-on-close job ends the tree anyway).
	sess, kill := c.sess, c.kill
	go func() {
		_ = sess.Close()
		if kill != nil {
			kill() // browser children the server left behind
		}
	}()
	c.sess, c.kill = nil, nil
	c.opts.Log.Info("mcp: server stopped", "server", c.opts.Name)
}

func (c *Client) touchLocked() {
	if c.idle != nil {
		c.idle.Stop()
	}
	gen := c.gen
	c.idle = time.AfterFunc(c.opts.IdleStop, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.gen == gen {
			c.stopLocked()
		}
	})
}

// decode maps a tool result to out or a *ToolError.
func decode(tool string, res *mcp.CallToolResult, out any) error {
	raw, err := payload(res)
	if res.IsError {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err == nil && json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
			return &ToolError{Tool: tool, Code: e.Error.Code, Message: e.Error.Message}
		}
		return &ToolError{Tool: tool, Code: CodeError, Message: text(res)}
	}
	if out == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", tool, err)
	}
	return nil
}

// payload is the structured result, else the JSON text of the first content.
func payload(res *mcp.CallToolResult) ([]byte, error) {
	if res.StructuredContent != nil {
		return json.Marshal(res.StructuredContent)
	}
	t := strings.TrimSpace(text(res))
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		return []byte(t), nil
	}
	return nil, fmt.Errorf("no structured result: %.200s", t)
}

func text(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
