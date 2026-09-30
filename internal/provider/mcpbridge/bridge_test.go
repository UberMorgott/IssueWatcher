package mcpbridge_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge/mcptest"
)

func TestCallDecodesAndAddsFormat(t *testing.T) {
	fake := mcptest.New()
	fake.Handle("echo", func(args map[string]any) (any, error) {
		return map[string]any{"format": args["format"], "n": args["n"]}, nil
	})
	b := mcpbridge.New(mcpbridge.Options{Name: "t", Dial: fake.Dial})
	defer b.Close()
	var out struct {
		Format string `json:"format"`
		N      int    `json:"n"`
	}
	if err := b.Call(context.Background(), "echo", map[string]any{"n": 7}, &out, true); err != nil {
		t.Fatal(err)
	}
	if out.Format != "json" || out.N != 7 {
		t.Fatalf("got %+v", out)
	}
}

func TestToolErrorCodes(t *testing.T) {
	fake := mcptest.New()
	fake.Handle("read", func(map[string]any) (any, error) {
		return nil, &mcptest.CodeError{Code: mcpbridge.CodeNotLoggedIn, Message: "log in"}
	})
	b := mcpbridge.New(mcpbridge.Options{Name: "t", Dial: fake.Dial})
	defer b.Close()
	err := b.Call(context.Background(), "read", nil, nil, true)
	if !mcpbridge.IsCode(err, mcpbridge.CodeNotLoggedIn) {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(mcpbridge.ProviderError(err, time.Now()), provider.ErrNotSignedIn) {
		t.Fatal("not_logged_in must map to provider.ErrNotSignedIn")
	}
	if _, ok := errors.AsType[*provider.RateLimitError](mcpbridge.ProviderError(&mcpbridge.ToolError{Code: mcpbridge.CodeRateLimited}, time.Now())); !ok {
		t.Fatal("rate_limited must map to *provider.RateLimitError")
	}
	if fake.Starts() != 1 {
		t.Fatalf("a tool error must not restart the server: %d starts", fake.Starts())
	}
}

func TestCrashRestartsOnceAndRetriesReads(t *testing.T) {
	fake := mcptest.New()
	var n atomic.Int32
	fake.Handle("read", func(map[string]any) (any, error) {
		if n.Add(1) == 1 {
			return nil, mcptest.ErrCrash
		}
		return map[string]any{"ok": true}, nil
	})
	b := mcpbridge.New(mcpbridge.Options{Name: "t", Dial: fake.Dial, CallTimeout: 5 * time.Second})
	defer b.Close()
	var out struct{ OK bool }
	if err := b.Call(context.Background(), "read", nil, &out, true); err != nil || !out.OK {
		t.Fatalf("read after crash: %v %+v", err, out)
	}
	if fake.Starts() != 2 {
		t.Fatalf("starts = %d, want 2 (one restart)", fake.Starts())
	}

	// A server that keeps crashing: one restart, then the error.
	fake.Handle("read", func(map[string]any) (any, error) { return nil, mcptest.ErrCrash })
	b2 := mcpbridge.New(mcpbridge.Options{Name: "t", Dial: fake.Dial, CallTimeout: 5 * time.Second})
	defer b2.Close()
	before := fake.Starts()
	if err := b2.Call(context.Background(), "read", nil, nil, true); err == nil {
		t.Fatal("want an error")
	}
	if got := fake.Starts() - before; got != 2 {
		t.Fatalf("starts = %d, want 2", got)
	}
}

func TestWriteNeverRetried(t *testing.T) {
	fake := mcptest.New()
	fake.Handle("post", func(map[string]any) (any, error) { return nil, mcptest.ErrCrash })
	b := mcpbridge.New(mcpbridge.Options{Name: "t", Dial: fake.Dial, CallTimeout: 5 * time.Second})
	defer b.Close()
	err := b.Call(context.Background(), "post", nil, nil, false)
	if !errors.Is(err, mcpbridge.ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrOutcomeUnknown", err)
	}
	if fake.Count("post") != 1 {
		t.Fatalf("post called %d times", fake.Count("post"))
	}
}

func TestMissingServerUnavailable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "index.js")
	b := mcpbridge.New(mcpbridge.Options{Name: "t", Command: func() (string, []string) { return "node", []string{missing} }})
	err := b.Call(context.Background(), "x", nil, nil, true)
	if !errors.Is(err, mcpbridge.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	b = mcpbridge.New(mcpbridge.Options{Name: "t", Command: func() (string, []string) { return "", nil }})
	if err := b.Call(context.Background(), "x", nil, nil, true); !errors.Is(err, mcpbridge.ErrUnavailable) {
		t.Fatalf("empty command: %v", err)
	}
}

func TestIdleStop(t *testing.T) {
	fake := mcptest.New()
	fake.Handle("read", func(map[string]any) (any, error) { return map[string]any{}, nil })
	b := mcpbridge.New(mcpbridge.Options{Name: "t", Dial: fake.Dial, IdleStop: 50 * time.Millisecond})
	defer b.Close()
	if err := b.Call(context.Background(), "read", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for b.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Running() {
		t.Fatal("server still running after the idle timeout")
	}
	if err := b.Call(context.Background(), "read", nil, nil, true); err != nil || fake.Starts() != 2 {
		t.Fatalf("lazy restart: %v, starts %d", err, fake.Starts())
	}
}

// A closed client (platform switched off) never starts a new server; Stop
// (changed command) does restart on the next call.
func TestCloseIsFinalStopIsNot(t *testing.T) {
	fake := mcptest.New()
	fake.Handle("read", func(map[string]any) (any, error) { return map[string]any{}, nil })
	b := mcpbridge.New(mcpbridge.Options{Name: "t", Dial: fake.Dial})
	if err := b.Call(t.Context(), "read", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	b.Stop()
	if err := b.Call(t.Context(), "read", nil, nil, true); err != nil || fake.Starts() != 2 {
		t.Fatalf("after Stop: %v, starts %d", err, fake.Starts())
	}
	b.Close()
	if err := b.Call(t.Context(), "read", nil, nil, true); !errors.Is(err, mcpbridge.ErrUnavailable) {
		t.Fatalf("after Close: %v", err)
	}
	if fake.Starts() != 2 || b.Running() {
		t.Fatalf("closed client started a server: starts %d", fake.Starts())
	}
}

// Waiting behind a long call ends with the caller's context, and Close does
// not wait for the in-flight call.
func TestWaitHonoursContextAndCloseDoesNotBlock(t *testing.T) {
	fake := mcptest.New()
	release := make(chan struct{})
	entered := make(chan struct{})
	fake.Handle("slow", func(map[string]any) (any, error) {
		close(entered)
		<-release
		return map[string]any{}, nil
	})
	b := mcpbridge.New(mcpbridge.Options{Name: "t", Dial: fake.Dial, CallTimeout: 10 * time.Second})
	done := make(chan error, 1)
	go func() { done <- b.Call(context.Background(), "slow", nil, nil, true) }()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.Call(ctx, "slow", nil, nil, true); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("queued call: %v after %v", err, time.Since(start))
	}
	closed := make(chan struct{})
	go func() { b.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for the in-flight call")
	}
	close(release)
	<-done // may finish or fail; it must not start a new server
	if err := b.Call(t.Context(), "slow", nil, nil, true); !errors.Is(err, mcpbridge.ErrUnavailable) || fake.Starts() != 1 {
		t.Fatalf("after Close: %v, starts %d", err, fake.Starts())
	}
}

func TestWriteUnsure(t *testing.T) {
	for code, want := range map[string]bool{
		mcpbridge.CodeOutcomeUnknown: true, mcpbridge.CodeError: true,
		mcpbridge.CodeNotLoggedIn: false, mcpbridge.CodeCloudflare: false, mcpbridge.CodeInvalid: false,
		mcpbridge.CodeDisabled: false, mcpbridge.CodeRateLimited: false, mcpbridge.CodeNotFound: false,
	} {
		if got := mcpbridge.WriteUnsure(&mcpbridge.ToolError{Code: code}); got != want {
			t.Errorf("%s: %v", code, got)
		}
	}
	if !mcpbridge.WriteUnsure(mcpbridge.ErrOutcomeUnknown) || mcpbridge.WriteUnsure(errors.New("x")) || mcpbridge.WriteUnsure(nil) {
		t.Fatal("plain errors")
	}
}
