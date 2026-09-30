package nexus

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/browser"
)

var tokenRe = regexp.MustCompile(`(data-csrf-token|name="_token" value)="[^"]*"`)

// TestCaptureFixtures saves live widget answers as test fixtures (read-only):
// IW_NEXUS_CAPTURE=1. Tokens are scrubbed.
func TestCaptureFixtures(t *testing.T) {
	if os.Getenv("IW_NEXUS_CAPTURE") != "1" {
		t.Skip("set IW_NEXUS_CAPTURE=1")
	}
	br := browser.New(browser.Options{Dir: t.TempDir(), Origins: Origins})
	defer br.Close()
	n := newNative(NativeOptions{Browser: br}, nil, time.Now)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	save := func(name, body string) {
		body = tokenRe.ReplaceAllString(body, `$1="TOKEN"`)
		if err := os.WriteFile(filepath.Join("testdata", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll("testdata", 0o750)
	gid, err := n.gameID(ctx, "windrose")
	if err != nil {
		t.Fatal(err)
	}
	tid, err := n.threadID(ctx, "windrose", 147)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []int{1, 2} {
		u := n.opts.Site + "/Core/Libs/Common/Widgets/CommentContainer?RH_CommentContainer=game_id:" + strconv.Itoa(gid) +
			",object_id:147,object_type:1,thread_id:" + strconv.Itoa(tid) + ",tabbed:1,skip_opening_post:0,page:" + strconv.Itoa(page)
		body, err := n.page(ctx, u, n.modPage("windrose", 147), nil)
		if err != nil {
			t.Fatal(err)
		}
		save("comments_"+strconv.Itoa(page)+".html", body)
	}
	// A mod with bug reports: SkyUI (Skyrim SE).
	sgid, err := n.gameID(ctx, "skyrimspecialedition")
	if err != nil {
		t.Fatal(err)
	}
	var body string
	var r bugsResult
	for _, id := range []int{3863, 17230, 2347, 34705, 32444, 12604, 1137, 17857} {
		u := n.opts.Site + "/Core/Libs/Common/Widgets/ModBugsTab?RH_ModBugsTab=game_id:" + strconv.Itoa(sgid) + ",id:" + strconv.Itoa(id) + ",page_size:10,page:1"
		body, err = n.page(ctx, u, n.modPage("skyrimspecialedition", id), nil)
		if err != nil {
			t.Fatal(err)
		}
		if r, _, err = parseBugs(body, "", 1); err == nil && len(r.Bugs) > 0 {
			t.Logf("bugs from skyrimspecialedition/%d", id)
			break
		}
	}
	save("bugs_1.html", body)
	if len(r.Bugs) == 0 {
		t.Fatal("no mod with bug reports found")
	}
	for i, b := range r.Bugs {
		if i >= 2 {
			break
		}
		body, err := n.page(ctx, n.opts.Site+"/Core/Libs/Common/Widgets/ModBugReplyList", "", url.Values{"issue_id": {b.ID}})
		if err != nil {
			t.Fatal(err)
		}
		save("bug_"+strconv.Itoa(i)+".html", body)
	}
	body, err = n.page(ctx, n.opts.Site+"/Core/Libs/Common/Widgets/ModBugReplyList", "", url.Values{"issue_id": {"1"}})
	if err == nil {
		save("bug_deleted.html", body)
	}
}
