package steam

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

func TestParseCommentsFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "render_0.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r renderPage
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.TotalCount != 23 || r.TimeLastPost != 1790599900 {
		t.Fatalf("fixture header %+v", r)
	}
	cs, err := parseComments(r.HTML)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 10 {
		t.Fatalf("comments %d, want 10", len(cs))
	}
	c := cs[0]
	if c.ID != "583935357196866537" || c.CreatedAt.Unix() != 1790599900 ||
		c.Body != "Does this mod work for Mutoid Soldiers as well?" || c.SteamID == ownerID || c.Author == "" {
		t.Fatalf("first comment %+v", c)
	}
	if cs[1].SteamID != ownerID || cs[1].Author != "Morgott" {
		t.Fatalf("owner comment %+v", cs[1])
	}
	for _, c := range cs {
		if strings.Contains(c.Body, "<") || strings.Contains(c.Body, "&nbsp;") {
			t.Fatalf("markup left in %q", c.Body)
		}
	}
	if _, err := parseComments(`<div class="commentthread_comment" id="comment_1"><div>no timestamp</div></div>`); err == nil {
		t.Fatal("comment without timestamp accepted")
	}
}

func TestBodyTextMarkup(t *testing.T) {
	cs, err := parseComments(`<div class="commentthread_comment" id="comment_7">
		<a class="commentthread_author_link" href="x" data-miniprofile="5"><bdi>A &amp; B</bdi></a>
		<div data-timestamp="1700000000"></div>
		<div class="commentthread_comment_text">
			Line one<br>1. step &lt;x&gt;<br><a class="bb_link" href="https://steamcommunity.com/linkfilter/?u=https://example.com/a">example</a>
			<img class="emoticon" alt=":steamhappy:"> <blockquote class="bb_blockquote">quoted</blockquote>after
		</div></div>`)
	if err != nil {
		t.Fatal(err)
	}
	want := "Line one\n1. step <x>\nexample (https://example.com/a) :steamhappy:\n> quoted\nafter"
	if cs[0].Body != want || cs[0].Author != "A & B" || cs[0].SteamID != "76561197960265733" {
		t.Fatalf("got %q / %q / %s", cs[0].Body, cs[0].Author, cs[0].SteamID)
	}
}

func TestListProjectsProfilePage(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	ps, err := p.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, pr := range ps {
		got[pr.ExternalID] = pr.Name
		if pr.URL != f.srv.URL+"/sharedfiles/filedetails/?id="+pr.ExternalID {
			t.Fatalf("url %s", pr.URL)
		}
	}
	if len(ps) != 3 || got["3796708495"] != "Renderforge" || got[fileID] == "" {
		t.Fatalf("projects %v", got)
	}
	if n := f.count("/ISteamRemoteStorage/GetPublishedFileDetails/v1/"); n != 1 {
		t.Fatalf("creator lookups %d, want 1 batched", n)
	}
	// A page without items or counter (private profile, changed markup) is an
	// error: an empty list would deactivate every known project.
	f.profilePage = "<html><body>This profile is private.</body></html>"
	if _, err := p.ListProjects(context.Background()); err == nil {
		t.Fatal("unrecognised page accepted")
	}
}

func TestSyncItemsPagingAndSince(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	pr := provider.Project{ExternalID: fileID, Name: "Oracle"}
	items, err := p.SyncItems(context.Background(), pr, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 23 || len(f.renders) != 3 || f.renders[2] != [2]int{20, 10} {
		t.Fatalf("items %d renders %v", len(items), f.renders)
	}
	for i, it := range items {
		if it.Number != i+1 || it.Kind != "comment" || !it.Open || it.Title == "" || !strings.HasPrefix(it.ExternalID, "comment:"+fileID+"/") {
			t.Fatalf("item %d: %+v", i, it)
		}
		if i > 0 && it.CreatedAt.Before(items[i-1].CreatedAt) {
			t.Fatal("not oldest first")
		}
	}
	newest := items[22]
	if newest.ExternalID != ItemExternalID(fileID, "583935357196866537") || newest.CreatedAt.Unix() != 1790599900 {
		t.Fatalf("newest %+v", newest)
	}
	if items[21].Author != ownerID { // own comments carry the account: no notification
		t.Fatalf("owner author %q", items[21].Author)
	}
	// Since the newest known comment: one page, only comments at/after it.
	f.renders = nil
	items, err = p.SyncItems(context.Background(), pr, items[20].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.renders) != 1 || len(items) != 3 || items[2].Number != 23 {
		t.Fatalf("since: renders %v items %d", f.renders, len(items))
	}
}

func TestDetectChanges(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	pr := provider.Project{ExternalID: fileID}
	var st provider.PollState
	ch, err := p.DetectChanges(context.Background(), pr, &st)
	if err != nil || !ch.Overflow || ch.Requests != 1 {
		t.Fatalf("first check %+v %v", ch, err)
	}
	ch, err = p.DetectChanges(context.Background(), pr, &st)
	if err != nil || ch.Overflow || ch.NotModified != 1 {
		t.Fatalf("unchanged %+v %v", ch, err)
	}
	if f.renders[len(f.renders)-1] != [2]int{0, 1} {
		t.Fatalf("check reads %v", f.renders)
	}
	f.mu.Lock()
	f.timeLastPost++
	f.mu.Unlock()
	if ch, err = p.DetectChanges(context.Background(), pr, &st); err != nil || !ch.Overflow {
		t.Fatalf("new post %+v %v", ch, err)
	}
}

func TestRateLimited(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	f.rateLimit = 1
	_, err := p.SyncItems(context.Background(), provider.Project{ExternalID: fileID}, time.Time{})
	var rl *provider.RateLimitError
	if !errors.As(err, &rl) || !rl.Secondary || rl.Reset.IsZero() {
		t.Fatalf("err %v", err)
	}
}

func TestSettingsProtectedAndValidated(t *testing.T) {
	dir := t.TempDir()
	p := New(Options{Dir: dir})
	if _, err := p.Account(context.Background()); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("unconfigured account err %v", err)
	}
	bad := "76561"
	if _, err := p.Save(Update{SteamID: &bad}); !errors.Is(err, ErrBadSettings) {
		t.Fatalf("bad id err %v", err)
	}
	url := "https://steamcommunity.com/profiles/" + ownerID + "/"
	key := strings.Repeat("A1", 16)
	ls, sid := "7656119799621059%7C%7CeyJ0eXAi.secret", "abcdef0123456789abcdef01"
	st, err := p.Save(Update{SteamID: &url, APIKey: &key, LoginSecure: &ls, SessionID: &sid})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Configured || st.SteamID != ownerID || !st.HasAPIKey || st.Session != SessionStored {
		t.Fatalf("status %+v", st)
	}
	raw, err := os.ReadFile(filepath.Join(dir, settingsFile)) //nolint:gosec // G304: test temp file
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{ls, sid, key} {
		if strings.Contains(string(raw), s) {
			t.Fatal("secret in plaintext on disk")
		}
	}
	p2 := New(Options{Dir: dir}) // a restart reads the DPAPI file back
	if acc, err := p2.Account(context.Background()); err != nil || acc != ownerID {
		t.Fatalf("reload %q %v", acc, err)
	}
	if st2, _ := p2.Status(); st2 != st {
		t.Fatalf("reload status %+v vs %+v", st2, st)
	}
}
