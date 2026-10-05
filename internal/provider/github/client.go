package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// minRemaining keeps a reserve of the hourly quota for user actions (replies).
const minRemaining = 50

// client does authenticated REST and GraphQL calls and tracks the rate limit
// from the x-ratelimit-* headers (REST and GraphQL both send them).
type client struct {
	auth *Auth
	http *http.Client
	now  func() time.Time

	mu        sync.Mutex
	limit     int
	remaining int
	reset     time.Time
	known     bool
}

func (c *client) checkQuota(reserve int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known && c.remaining < reserve && c.now().Before(c.reset) {
		return &provider.RateLimitError{Reset: c.reset}
	}
	return nil
}

func (c *client) track(h http.Header) {
	rem, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	reset, err2 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	limit, _ := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	c.mu.Lock()
	c.remaining, c.reset, c.known = rem, time.Unix(reset, 0), true
	if limit > 0 {
		c.limit = limit
	}
	c.mu.Unlock()
}

// rate is the last reported quota.
func (c *client) rate() (provider.RateStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return provider.RateStatus{Limit: c.limit, Remaining: c.remaining, Reset: c.reset}, c.known
}

// do sends an authenticated JSON request. reserve is the quota to keep back
// (sync keeps minRemaining, user actions 0).
func (c *client) do(ctx context.Context, method, u string, in, out any, reserve int) error {
	if err := c.checkQuota(reserve); err != nil {
		return err
	}
	token, err := c.auth.AccessToken(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	c.track(resp.Header)
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if err := c.status(resp, data, req); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: decode %s: %w", req.URL.Path, err)
	}
	return nil
}

// status maps an error response: 401 signs out, 429/403 rate limits (primary
// or secondary), anything else non-2xx is an error.
func (c *client) status(resp *http.Response, data []byte, req *http.Request) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		_ = c.auth.Logout() // revoked or expired beyond refresh
		return fmt.Errorf("%w: %s", provider.ErrNotSignedIn, resp.Status)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden:
		secondary := strings.Contains(strings.ToLower(string(data)), "secondary rate limit")
		if resp.StatusCode == http.StatusTooManyRequests || secondary ||
			resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return &provider.RateLimitError{Reset: c.retryAt(resp.Header), Secondary: secondary || resp.Header.Get("X-RateLimit-Remaining") != "0"}
		}
		return fmt.Errorf("github: %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, truncate(string(data), 300))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &statusError{code: resp.StatusCode,
			msg: fmt.Sprintf("github: %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, truncate(string(data), 300))}
	}
	return nil
}

// statusError is a non-2xx answer other than a sign-out or a rate limit.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// isNotFound reports a 404 answer.
func isNotFound(err error) bool {
	se, ok := errors.AsType[*statusError](err)
	return ok && se.code == http.StatusNotFound
}

func (c *client) retryAt(h http.Header) time.Time {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		return c.now().Add(time.Duration(s) * time.Second)
	}
	if r, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		return time.Unix(r, 0)
	}
	return c.now().Add(time.Minute)
}

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"` // the failed field, e.g. ["repository", "i0"]
}

// graphql runs query with vars and decodes data into out; any error fails it.
func (c *client) graphql(ctx context.Context, query string, vars map[string]any, out any, reserve int) error {
	errs, err := c.graphqlPartial(ctx, query, vars, out, reserve)
	if err == nil && len(errs) > 0 {
		err = errors.New("github graphql: " + errs[0].Message)
	}
	return err
}

// graphqlPartial is graphql for queries whose fields can fail one by one (a
// NOT_FOUND alias comes back null next to an error naming its path): data is
// decoded and the errors are returned for the caller to sort out.
func (c *client) graphqlPartial(ctx context.Context, query string, vars map[string]any, out any, reserve int) ([]gqlError, error) {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	in := map[string]any{"query": query, "variables": vars}
	if err := c.do(ctx, http.MethodPost, c.auth.APIURL+"/graphql", in, &resp, reserve); err != nil {
		return nil, err
	}
	for _, e := range resp.Errors {
		if e.Type == "RATE_LIMITED" {
			return nil, &provider.RateLimitError{Reset: c.now().Add(time.Minute)}
		}
	}
	if len(resp.Errors) > 0 && (len(resp.Data) == 0 || string(resp.Data) == "null") {
		return nil, errors.New("github graphql: " + resp.Errors[0].Message)
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return nil, fmt.Errorf("github graphql: decode: %w", err)
	}
	return resp.Errors, nil
}

// condResult is a conditional GET's answer.
type condResult struct {
	notModified  bool
	etag         string
	body         []byte
	pollInterval time.Duration // X-Poll-Interval
}

// conditional GETs u with If-None-Match: etag. A 304 has no body and, for an
// authenticated request, does not count against the primary rate limit.
func (c *client) conditional(ctx context.Context, u, etag string) (condResult, error) {
	var res condResult
	if err := c.checkQuota(minRemaining); err != nil {
		return res, err
	}
	token, err := c.auth.AccessToken(ctx)
	if err != nil {
		return res, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return res, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return res, err
	}
	defer func() { _ = resp.Body.Close() }()
	c.track(resp.Header)
	if s, err := strconv.Atoi(resp.Header.Get("X-Poll-Interval")); err == nil && s > 0 {
		res.pollInterval = time.Duration(s) * time.Second
	}
	if resp.StatusCode == http.StatusNotModified {
		res.notModified = true
		return res, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return res, err
	}
	if err := c.status(resp, data, req); err != nil {
		return res, err
	}
	res.etag, res.body = resp.Header.Get("ETag"), data
	return res, nil
}
