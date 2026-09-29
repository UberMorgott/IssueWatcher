// Package control is a thin client of the running app's loopback HTTP API for
// the CLI subcommands and the MCP server (docs/ARCHITECTURE.md → Control). It
// has no store, provider or auth of its own and never starts the app.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/instance"
)

// Default deadlines of one API call: reads are quick; a publishing call (push
// with the user's hooks, bounded by the runner's 5 min git timeout, then
// GitHub) may take minutes, and cutting it off leaves its outcome unknown.
const (
	ReadTimeout    = 10 * time.Second
	PublishTimeout = 6 * time.Minute
)

// maxResponse bounds a response body (job logs are the largest).
const maxResponse = 32 << 20

// ErrNotRunning: no running app for the data dir (no runtime.json, or nothing
// listens on its port any more).
var ErrNotRunning = errors.New("IssueWatcher is not running (start the app first)")

// ErrOutcomeUnknown: a POST was sent but no answer came back (deadline, lost
// connection). The app may have done it (posted the comment, pushed): check
// the item or job before retrying.
var ErrOutcomeUnknown = errors.New("no answer from the app: it may have done it anyway, check the item or job state before retrying")

// APIError is a non-2xx answer of the API; Message is its {error} text.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

// Client talks to the app that owns DataDir.
type Client struct {
	DataDir string
	// Deadlines of GET and of POST calls (New sets ReadTimeout, PublishTimeout).
	ReadTimeout, PublishTimeout time.Duration
	http                        *http.Client
}

// New returns a client for the app owning dataDir.
func New(dataDir string) *Client {
	return &Client{DataDir: dataDir, ReadTimeout: ReadTimeout, PublishTimeout: PublishTimeout, http: &http.Client{
		// Never follow a redirect: the token must only reach the app itself.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{Proxy: nil}, // loopback only: never through a proxy
	}}
}

// base reads runtime.json fresh (a restarted app has a new port and token) and
// accepts only http://127.0.0.1:<port>.
func (c *Client) base() (string, string, error) {
	rt, err := instance.ReadRuntime(c.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", ErrNotRunning
	}
	if err != nil {
		return "", "", err
	}
	want := "http://127.0.0.1:" + strconv.Itoa(rt.Port)
	u, err := url.Parse(rt.URL)
	if err != nil || rt.Port <= 0 || rt.Port > 65535 || u.Scheme+"://"+u.Host != want || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.User != nil {
		return "", "", fmt.Errorf("control: runtime.json has a non-loopback URL %q", rt.URL)
	}
	if rt.Token == "" {
		return "", "", errors.New("control: runtime.json has no token")
	}
	return want, rt.Token, nil
}

// Do sends one request; in is JSON-encoded (nil = no body), a 2xx body is
// returned as is (nil for an empty one). Errors: ErrNotRunning, *APIError.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, in any) (json.RawMessage, error) {
	base, token, err := c.base()
	if err != nil {
		return nil, err
	}
	u := base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	timeout := c.ReadTimeout
	if method != http.MethodGet {
		timeout = c.PublishTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || isRefused(err) {
			return nil, ErrNotRunning // stale runtime.json of a crashed app
		}
		if method != http.MethodGet {
			return nil, fmt.Errorf("control: %s %s: %w (%w)", method, path, ErrOutcomeUnknown, err)
		}
		return nil, fmt.Errorf("control: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("control: read %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		msg := resp.Status
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		} else if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			msg = "unexpected redirect to " + resp.Header.Get("Location")
		}
		return nil, &APIError{Status: resp.StatusCode, Message: msg}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	return b, nil
}

// isRefused reports a refused TCP connect (Windows: WSAECONNREFUSED).
func isRefused(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		return false
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == 10061 // WSAECONNREFUSED
}

func (c *Client) get(ctx context.Context, path string, q url.Values) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodGet, path, q, nil)
}

func (c *Client) post(ctx context.Context, path string, in any) (json.RawMessage, error) {
	return c.Do(ctx, http.MethodPost, path, nil, in)
}

func idPath(format string, id int64) string { return fmt.Sprintf(format, id) }

// Status: health (version, port) + sync status.
func (c *Client) Status(ctx context.Context) (json.RawMessage, error) {
	health, err := c.get(ctx, "/api/health", nil)
	if err != nil {
		return nil, err
	}
	sync, err := c.get(ctx, "/api/sync", nil)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"running": true, "health": health, "sync": sync})
}

// Projects lists all projects with counts.
func (c *Client) Projects(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/api/projects", nil)
}

// ItemQuery are the GET /api/items filters (zero = unset).
type ItemQuery struct {
	Project int64
	State   string
	Label   string
	Text    string
	Unread  bool
	Limit   int
	Cursor  string
}

// Items lists one keyset chunk of items.
func (c *Client) Items(ctx context.Context, f ItemQuery) (json.RawMessage, error) {
	q := url.Values{}
	setInt(q, "project", f.Project)
	set(q, "state", f.State)
	set(q, "label", f.Label)
	set(q, "q", f.Text)
	if f.Unread {
		q.Set("unread", "1")
	}
	setInt(q, "limit", int64(f.Limit))
	set(q, "cursor", f.Cursor)
	return c.get(ctx, "/api/items", q)
}

// Item returns {item, comments}: the item with body and the first chunk of
// its comments (oldest first, commentLimit rows; 0 = the API default).
func (c *Client) Item(ctx context.Context, id int64, commentLimit int) (json.RawMessage, error) {
	item, err := c.get(ctx, idPath("/api/items/%d", id), nil)
	if err != nil {
		return nil, err
	}
	comments, err := c.Comments(ctx, id, commentLimit, "")
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"item": item, "comments": comments})
}

// Comments returns one chunk {items, nextCursor, more} of the item's comments,
// oldest first, after cursor ("" = from the first; limit 0 = the API default).
func (c *Client) Comments(ctx context.Context, id int64, limit int, cursor string) (json.RawMessage, error) {
	q := url.Values{}
	setInt(q, "limit", int64(limit))
	set(q, "cursor", cursor)
	return c.get(ctx, idPath("/api/items/%d/comments", id), q)
}

// JobQuery are the GET /api/jobs filters (zero = unset).
type JobQuery struct {
	State   string
	Flow    string
	Origin  string
	Project int64
	Item    int64
	Limit   int
	Cursor  string
}

// Jobs lists one keyset chunk of jobs, newest first.
func (c *Client) Jobs(ctx context.Context, f JobQuery) (json.RawMessage, error) {
	q := url.Values{}
	set(q, "state", f.State)
	set(q, "flow", f.Flow)
	set(q, "origin", f.Origin)
	setInt(q, "project", f.Project)
	setInt(q, "item", f.Item)
	setInt(q, "limit", int64(f.Limit))
	set(q, "cursor", f.Cursor)
	return c.get(ctx, "/api/jobs", q)
}

// Job returns one job.
func (c *Client) Job(ctx context.Context, id int64) (json.RawMessage, error) {
	return c.get(ctx, idPath("/api/jobs/%d", id), nil)
}

// JobLog returns {attempt, steps} (attempt 0 = the current one).
func (c *Client) JobLog(ctx context.Context, id int64, attempt int) (json.RawMessage, error) {
	q := url.Values{}
	setInt(q, "attempt", int64(attempt))
	return c.get(ctx, idPath("/api/jobs/%d/log", id), q)
}

// Sync asks the app to sync now (asynchronous).
func (c *Client) Sync(ctx context.Context) error {
	_, err := c.post(ctx, "/api/sync", nil)
	return err
}

// Reply posts a comment on the item's platform issue.
func (c *Client) Reply(ctx context.Context, itemID int64, body string) (json.RawMessage, error) {
	return c.post(ctx, idPath("/api/items/%d/comments", itemID), map[string]string{"body": body})
}

// CreateJobs queues one flow job per item → {jobs:[{itemId, job?, error?}]}.
func (c *Client) CreateJobs(ctx context.Context, flow string, itemIDs []int64, profileID string) (json.RawMessage, error) {
	return c.post(ctx, "/api/jobs", map[string]any{"itemIds": itemIDs, "flow": flow, "profileId": profileID})
}

// JobActions are the POST /api/jobs/{id}/<action> endpoints without a body.
var JobActions = []string{"cancel", "retry", "dismiss", "push", "pr"}

// JobAction runs one of JobActions.
func (c *Client) JobAction(ctx context.Context, id int64, action string) (json.RawMessage, error) {
	return c.post(ctx, fmt.Sprintf("/api/jobs/%d/%s", id, url.PathEscape(action)), nil)
}

// JobReply posts the (edited) reply draft of a reply job.
func (c *Client) JobReply(ctx context.Context, id int64, body string) (json.RawMessage, error) {
	return c.post(ctx, idPath("/api/jobs/%d/reply", id), map[string]string{"body": body})
}

// JobLabels adds labels to a label job's issue (checked, add only).
func (c *Client) JobLabels(ctx context.Context, id int64, labels []string) (json.RawMessage, error) {
	return c.post(ctx, idPath("/api/jobs/%d/labels", id), map[string][]string{"labels": labels})
}

func set(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

func setInt(q url.Values, k string, v int64) {
	if v != 0 {
		q.Set(k, strconv.FormatInt(v, 10))
	}
}
