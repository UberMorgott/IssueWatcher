package nexus

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
)

// TestCrossCheckMCP compares the native engine with the MCP server on a public
// mod with bug reports (read-only): IW_NEXUS_MCP=<path to build\index.js>.
func TestCrossCheckMCP(t *testing.T) {
	script := os.Getenv("IW_NEXUS_MCP")
	if script == "" {
		t.Skip("set IW_NEXUS_MCP")
	}
	bridge := mcpbridge.New(mcpbridge.Options{Name: "nexus", Command: func() (string, []string) { return "node", []string{script} }, CallTimeout: 2 * time.Minute})
	defer bridge.Close()
	br := browser.New(browser.Options{Dir: t.TempDir(), Origins: Origins})
	defer br.Close()
	old := &mcpBackend{bridge: bridge, now: time.Now}
	nat := newNative(NativeOptions{Browser: br}, nil, time.Now)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, pg := range []int{1, 2} {
		a, err := old.bugs(ctx, "skyrimspecialedition", 2347, pg)
		if err != nil {
			t.Fatal(err)
		}
		b, err := nat.bugs(ctx, "skyrimspecialedition", 2347, pg)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("bugs page %d differ:\nmcp    %+v\nnative %+v", pg, a, b)
		}
		for i, row := range a.Bugs {
			if i >= 4 {
				break
			}
			x, err := old.bug(ctx, atoi(row.ID))
			if err != nil {
				t.Fatal(err)
			}
			y, err := nat.bug(ctx, atoi(row.ID))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(x, y) {
				t.Fatalf("bug %s differs:\nmcp    %+v\nnative %+v", row.ID, x, y)
			}
		}
		t.Logf("page %d: %d bugs equal", pg, len(a.Bugs))
	}
	a, err := old.comments(ctx, "skyrimspecialedition", 2347, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := nat.comments(ctx, "skyrimspecialedition", 2347, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("comments differ:\nmcp    %+v\nnative %+v", a, b)
	}
	t.Logf("comments page 1: %d threads equal (total %d, pages %d)", len(a.Comments), a.Total, a.Pages)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
