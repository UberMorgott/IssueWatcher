package nexus_test

import (
	"context"
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
		return nil, &mcptest.CodeError{Code: "invalid", Message: "text too long"}
	})
	if _, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "hi"); err == nil || errors.Is(err, nexus.ErrUnknownOutcome) {
		t.Fatalf("refusal: %v", err)
	}
	// Failed after the send without a refusal, nothing saved: unknown, never a plain failure.
	for _, code := range []string{"outcome_unknown", "error"} {
		e.fake.Handle("post_mod_comment", func(map[string]any) (any, error) {
			return nil, &mcptest.CodeError{Code: code, Message: "HTTP 500"}
		})
		if _, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "hi"); !errors.Is(err, nexus.ErrUnknownOutcome) {
			t.Fatalf("%s: %v", code, err)
		}
	}
	if n := e.fake.Count("post_mod_comment"); n != 4 {
		t.Fatalf("posts = %d (never retried)", n)
	}
}

// H1: the site answers 500 but saved the post (outcome_unknown): the read-back
// finds it, so the user is not invited to post it again.
func TestReplyServerUnknownOutcomeReadBack(t *testing.T) {
	e := setup(t)
	e.fake.Handle("reply_mod_bug", func(map[string]any) (any, error) {
		e.site.mu.Lock()
		bp := e.site.bugPosts["900"]
		bp["replies"] = append(bp["replies"].([]map[string]any), map[string]any{"id": "906", "parentId": "900", "author": "UberMorgott", //nolint:forcetypeassert // fixture
			"authorId": 1, "createdAt": nil, "createdAtLocal": "2026-09-01T14:00", "body": "on it"})
		e.site.mu.Unlock()
		return nil, &mcptest.CodeError{Code: "outcome_unknown", Message: "HTTP 500"}
	})
	e.fake.Handle("post_mod_comment", func(map[string]any) (any, error) {
		e.site.mu.Lock()
		e.site.pages[1][1]["replies"] = []map[string]any{reply("201", "102", "UberMorgott", "thanks!", 21), reply("311", "102", "UberMorgott", "ok", 59)}
		e.site.mu.Unlock()
		return nil, &mcptest.CodeError{Code: "outcome_unknown", Message: "HTTP 502"}
	})
	c, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "ok")
	if err != nil || c.ExternalID != "311" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err := e.prov.Reply(t.Context(), "bug:900", "on it"); err != nil || c.ExternalID != "906" {
		t.Fatalf("bug: %+v %v", c, err)
	}
}

// M4: an older reply with the same text is never taken for a post that did
// not land (it existed before the post).
func TestReplyReadBackIgnoresOlderIdenticalReply(t *testing.T) {
	e := setup(t)
	e.fake.Handle("post_mod_comment", func(map[string]any) (any, error) { return nil, mcptest.ErrCrash })
	if _, err := e.prov.Reply(t.Context(), "comment:windrose/147/102", "thanks!"); !errors.Is(err, nexus.ErrUnknownOutcome) {
		t.Fatalf("comment: %v", err)
	}
	e.fake.Handle("reply_mod_bug", func(map[string]any) (any, error) { return nil, mcptest.ErrCrash })
	if _, err := e.prov.Reply(t.Context(), "bug:900", "looking"); !errors.Is(err, nexus.ErrUnknownOutcome) {
		t.Fatalf("bug: %v", err)
	}
}

// L7: a cancelled request (closed tab) still reads the reply back.
func TestReplyReadBackSurvivesCancel(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	e.fake.Handle("post_mod_comment", func(map[string]any) (any, error) {
		e.site.mu.Lock()
		e.site.pages[1][1]["replies"] = []map[string]any{reply("201", "102", "UberMorgott", "thanks!", 21), reply("312", "102", "UberMorgott", "bye", 59)}
		e.site.mu.Unlock()
		cancel()
		return nil, mcptest.ErrCrash
	})
	if c, err := e.prov.Reply(ctx, "comment:windrose/147/102", "bye"); err != nil || c.ExternalID != "312" {
		t.Fatalf("%+v %v", c, err)
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
