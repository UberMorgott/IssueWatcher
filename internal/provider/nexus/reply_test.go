package nexus_test

import (
	"errors"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge/mcptest"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
)

func TestReplyComment(t *testing.T) {
	e := setup(t)
	e.fake.Handle("post_mod_comment", func(args map[string]any) (any, error) {
		if args["game"] != "windrose" || args["mod_id"] != float64(147) || args["parent_id"] != float64(102) || args["text"] != "hi" {
			t.Errorf("args %v", args)
		}
		return map[string]any{"posted": true, "dryRun": false, "id": "300", "parentId": "102", "verified": true, "httpStatus": 200}, nil
	})
	c, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "hi")
	if err != nil {
		t.Fatal(err)
	}
	if c.ExternalID != "300" || c.Author != "UberMorgott" || c.Body != "hi" {
		t.Fatalf("comment %+v", c)
	}
}

func TestReplyBug(t *testing.T) {
	e := setup(t)
	e.fake.Handle("reply_mod_bug", func(args map[string]any) (any, error) {
		if args["issue_id"] != float64(900) {
			t.Errorf("args %v", args)
		}
		return map[string]any{"posted": true, "dryRun": false, "id": "905", "parentId": "900", "verified": true, "httpStatus": 200}, nil
	})
	c, err := e.prov.Reply(t.Context(), "bug:900", "on it")
	if err != nil || c.ExternalID != "905" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestReplyErrors(t *testing.T) {
	e := setup(t)
	e.fake.Handle("post_mod_comment", func(map[string]any) (any, error) {
		return nil, &mcptest.CodeError{Code: "not_logged_in", Message: "log in"}
	})
	if _, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "hi"); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("not_logged_in: %v", err)
	}
	e.fake.Handle("post_mod_comment", func(map[string]any) (any, error) {
		return nil, &mcptest.CodeError{Code: "error", Message: "HTTP 500"}
	})
	if _, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "hi"); err == nil || errors.Is(err, nexus.ErrUnknownOutcome) {
		t.Fatalf("tool error: %v", err)
	}
	if n := e.fake.Count("post_mod_comment"); n != 2 {
		t.Fatalf("posts = %d (never retried)", n)
	}
}

// The answer is lost after the post: the thread is read back.
func TestReplyLostAnswerReadBack(t *testing.T) {
	e := setup(t)
	e.fake.Handle("post_mod_comment", func(map[string]any) (any, error) {
		e.site.mu.Lock()
		p2 := e.site.pages[2]
		p2[0]["replies"] = []map[string]any{reply("201", "102", "UberMorgott", "thanks!", 21), reply("310", "102", "UberMorgott", "fixed  in\n1.3.1", 60)}
		e.site.mu.Unlock()
		return nil, mcptest.ErrCrash
	})
	c, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "fixed in 1.3.1")
	if err != nil || c.ExternalID != "310" {
		t.Fatalf("%+v %v", c, err)
	}
	if n := e.fake.Count("post_mod_comment"); n != 1 {
		t.Fatalf("posts = %d", n)
	}
}

func TestReplyReadBackMissIsUnknown(t *testing.T) {
	e := setup(t)
	e.fake.Handle("post_mod_comment", func(map[string]any) (any, error) {
		return map[string]any{"posted": true, "dryRun": false, "id": nil, "parentId": "102", "verified": false, "httpStatus": 200}, nil
	})
	_, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "never seen")
	if !errors.Is(err, nexus.ErrUnknownOutcome) {
		t.Fatalf("err = %v", err)
	}
	e.fake.Handle("reply_mod_bug", func(map[string]any) (any, error) { return nil, mcptest.ErrCrash })
	if _, err := e.prov.Reply(t.Context(), "bug:900", "lost"); !errors.Is(err, nexus.ErrUnknownOutcome) {
		t.Fatalf("bug: %v", err)
	}
}
