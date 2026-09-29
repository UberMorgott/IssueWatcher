package control

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpSession(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func text(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) != 1 {
		t.Fatalf("content %+v", r.Content)
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content %T", r.Content[0])
	}
	return tc.Text
}

func TestMCPTools(t *testing.T) {
	var got []string
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(b))
		switch r.URL.Path {
		case "/api/items", "/api/items/4/comments":
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":"","more":false}`))
		case "/api/jobs/5/reply":
			_, _ = w.Write([]byte(`{"id":5,"state":"done"}`))
		case "/api/jobs/6/push":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"runner: job 6 is done"}`))
		case "/api/jobs/5/log":
			_, _ = w.Write([]byte(`{"attempt":1,"steps":[{"text":"a"},{"text":"b"},{"text":"c"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	cs := mcpSession(t, NewMCPServer(c, "test", slog.New(slog.DiscardHandler)))

	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	want := []string{"apply_job_labels", "cancel_job", "create_pr", "get_item", "get_job", "get_job_log", "list_item_comments", "list_items",
		"list_jobs", "list_projects", "push_job", "reply_item", "retry_job", "send_job_reply", "start_jobs", "sync_now"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools %v", names)
	}

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return r
	}
	if r := call("list_items", map[string]any{"limit": 500, "state": "open"}); r.IsError || !json.Valid([]byte(text(t, r))) {
		t.Fatalf("list_items: %+v", r)
	}
	if last := got[len(got)-1]; last != "GET /api/items?limit=50&state=open " {
		t.Fatalf("list_items request %q", last)
	}
	// Later comments of a long thread: the next pages after get_item.
	if r := call("list_item_comments", map[string]any{"id": 4, "cursor": "c1", "limit": 500}); r.IsError {
		t.Fatalf("list_item_comments: %+v", r)
	}
	if last := got[len(got)-1]; last != "GET /api/items/4/comments?cursor=c1&limit=50 " {
		t.Fatalf("list_item_comments request %q", last)
	}
	if r := call("send_job_reply", map[string]any{"id": 5, "body": "Thanks!"}); r.IsError || !strings.Contains(text(t, r), `"done"`) {
		t.Fatalf("send_job_reply: %+v", r)
	}
	if last := got[len(got)-1]; last != `POST /api/jobs/5/reply? {"body":"Thanks!"}` {
		t.Fatalf("send_job_reply request %q", last)
	}
	if r := call("push_job", map[string]any{"id": 6}); !r.IsError || !strings.Contains(text(t, r), "job 6 is done (HTTP 409)") {
		t.Fatalf("push_job: %+v", r)
	}
	var log struct {
		Total int `json:"total"`
		Steps []struct {
			Text string `json:"text"`
		} `json:"steps"`
	}
	r := call("get_job_log", map[string]any{"id": 5, "tail": 2})
	if err := json.Unmarshal([]byte(text(t, r)), &log); err != nil || log.Total != 3 || len(log.Steps) != 2 || log.Steps[1].Text != "c" {
		t.Fatalf("get_job_log: %s", text(t, r))
	}
	if r := call("start_jobs", map[string]any{"flow": "fix", "item_ids": []int64{}}); !r.IsError {
		t.Fatal("start_jobs without ids succeeded")
	}
}

// A job's server (mcp --item) reads only its own issue; no write tools.
func TestItemMCPTools(t *testing.T) {
	var got []string
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/api/items/7":
			_, _ = w.Write([]byte(`{"id":7,"comments":{"items":[],"more":false}}`))
		case "/api/items/7/comments":
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":"","more":false}`))
		default:
			http.NotFound(w, r)
		}
	})
	cs := mcpSession(t, NewItemMCPServer(c, 7, "test", slog.New(slog.DiscardHandler)))
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not read-only", tl.Name)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"get_item", "list_item_comments"}) {
		t.Fatalf("tools %v", names)
	}
	// No id argument: the item is fixed by the job.
	for name, args := range map[string]map[string]any{
		"get_item":           {"comments": 5},
		"list_item_comments": {"cursor": "c1"},
	} {
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || r.IsError {
			t.Fatalf("%s: %v %+v", name, err, r)
		}
	}
	slices.Sort(got)
	want := []string{"GET /api/items/7/comments?cursor=c1&limit=20", "GET /api/items/7/comments?limit=5", "GET /api/items/7?"}
	if !slices.Equal(got, want) {
		t.Fatalf("requests %q", got)
	}
}
