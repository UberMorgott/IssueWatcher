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
	c.mu.Lock()
	c.remaining, c.reset, c.known = rem, time.Unix(reset, 0), true
	c.mu.Unlock()
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
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		_ = c.auth.Logout() // revoked or expired beyond refresh
		return fmt.Errorf("%w: %s", provider.ErrNotSignedIn, resp.Status)
	case resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0")):
		return &provider.RateLimitError{Reset: c.retryAt(resp.Header)}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("github: %s %s: %s: %s", method, req.URL.Path, resp.Status, truncate(string(data), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: decode %s: %w", req.URL.Path, err)
	}
	return nil
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
}

// graphql runs query with vars and decodes data into out.
func (c *client) graphql(ctx context.Context, query string, vars map[string]any, out any, reserve int) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	in := map[string]any{"query": query, "variables": vars}
	if err := c.do(ctx, http.MethodPost, c.auth.APIURL+"/graphql", in, &resp, reserve); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		for _, e := range resp.Errors {
			if e.Type == "RATE_LIMITED" {
				return &provider.RateLimitError{Reset: c.now().Add(time.Minute)}
			}
		}
		return errors.New("github graphql: " + resp.Errors[0].Message)
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("github graphql: decode: %w", err)
	}
	return nil
}
