package control

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A triage job's server (mcp --project) reads only its project's issues: the
// list is pinned to the project, another project's item is refused before
// any comment is read; no write tools.
func TestProjectMCPTools(t *testing.T) {
	var got []string
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch r.URL.Path {
		case "/api/items":
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":"","more":false}`))
		case "/api/items/7":
			_, _ = w.Write([]byte(`{"id":7,"repoId":3,"title":"mine"}`))
		case "/api/items/8":
			_, _ = w.Write([]byte(`{"id":8,"repoId":4,"title":"foreign"}`))
		case "/api/items/7/comments", "/api/items/8/comments":
			_, _ = w.Write([]byte(`{"items":[],"nextCursor":"","more":false}`))
		default:
			http.NotFound(w, r)
		}
	})
	cs := mcpSession(t, NewProjectMCPServer(c, 3, "test", slog.New(slog.DiscardHandler)))
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
	if !slices.Equal(names, []string{"get_item", "list_item_comments", "list_items"}) {
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
	// The project argument does not exist: the list is always this project's (open by default).
	if r := call("list_items", map[string]any{"limit": 5}); r.IsError {
		t.Fatalf("list_items: %+v", r)
	}
	if r := call("list_items", map[string]any{"state": "all", "query": "#2"}); r.IsError {
		t.Fatalf("list_items all: %+v", r)
	}
	if r := call("get_item", map[string]any{"id": 7}); r.IsError || !strings.Contains(text(t, r), `"mine"`) {
		t.Fatalf("get_item own: %+v", r)
	}
	for name, args := range map[string]map[string]any{"get_item": {"id": 8}, "list_item_comments": {"id": 8}} {
		if r := call(name, args); !r.IsError || !strings.Contains(text(t, r), "not in this job's project") {
			t.Fatalf("%s foreign: %+v", name, r)
		}
	}
	if r := call("list_item_comments", map[string]any{"id": 7, "cursor": "c1"}); r.IsError {
		t.Fatalf("list_item_comments own: %+v", r)
	}
	for _, g := range got {
		if strings.HasPrefix(g, "GET /api/items/8/comments") {
			t.Fatalf("a foreign item's comments were read: %q", got)
		}
	}
	want := []string{"GET /api/items?limit=5&project=3&state=open", "GET /api/items?limit=20&project=3&q=%232"}
	if !slices.Equal(got[:2], want) {
		t.Fatalf("list requests %q, want %q", got[:2], want)
	}
}
