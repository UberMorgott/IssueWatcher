package steam

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

const (
	// target: the newest fixture comment, by an anonymised user.
	targetID  = "583935357196866537"
	ownerTmpl = "583935003344264502" // an owner comment used as the markup template
)

var commentText = regexp.MustCompile(`(id="comment_content_\d+">)[\s\S]*?(</div>)`)

// ownerBlock is a new comment by the owner in Steam's markup.
func (f *fakeSteam) ownerBlock(id, text string, at time.Time) string {
	var tmpl string
	for _, b := range f.blocks {
		if strings.Contains(b, `id="comment_`+ownerTmpl+`"`) {
			tmpl = b
		}
	}
	if tmpl == "" {
		f.t.Fatal("owner template block missing")
	}
	b := strings.ReplaceAll(tmpl, ownerTmpl, id)
	b = regexp.MustCompile(`data-timestamp="\d+"`).ReplaceAllString(b, fmt.Sprintf(`data-timestamp="%d"`, at.Unix()))
	return commentText.ReplaceAllString(b, "${1}"+strings.ReplaceAll(text, "\n", "<br>")+"${2}")
}

func (f *fakeSteam) signedIn(t *testing.T, p *Provider) {
	t.Helper()
	ls, sid := "76561197996210591%7C%7CeyJhbGciOi.test", "0123456789abcdef01234567"
	if _, err := p.Save(Update{LoginSecure: &ls, SessionID: &sid}); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeSteam) acceptPosts() {
	f.postReply = func(r *http.Request) (int, string) {
		f.blocks = append([]string{f.ownerBlock("900000000000000001", r.PostForm.Get("comment"), time.Now())}, f.blocks...)
		return http.StatusOK, fmt.Sprintf(`{"success":true,"name":"x","start":0,"pagesize":"10","total_count":%d,"timelastpost":%d,"comments_html":%q}`,
			len(f.blocks), time.Now().Unix(), strings.Join(f.blocks[:min(10, len(f.blocks))], ""))
	}
}

func TestReplyPostsExactFormAndCookies(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	f.signedIn(t, p)
	f.acceptPosts()
	c, err := p.Reply(context.Background(), ItemExternalID(fileID, targetID), "Yes, it does.\nSee the guide.")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 {
		t.Fatalf("posts %d", len(f.posts))
	}
	want := map[string]string{
		"comment": "@user1 Yes, it does.\nSee the guide.", "count": "10",
		"sessionid": "0123456789abcdef01234567", "feature2": "-1",
	}
	if got := f.postForms[0]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("form %v, want %v", got, want)
	}
	r := f.posts[0]
	if ck, err := r.Cookie("steamLoginSecure"); err != nil || ck.Value != "76561197996210591%7C%7CeyJhbGciOi.test" {
		t.Fatalf("steamLoginSecure %v %v", ck, err)
	}
	if ck, err := r.Cookie("sessionid"); err != nil || ck.Value != "0123456789abcdef01234567" {
		t.Fatalf("sessionid cookie %v %v", ck, err)
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		t.Fatalf("content type %q", r.Header.Get("Content-Type"))
	}
	if c.ExternalID != "900000000000000001" || c.Author != ownerID || c.Body != want["comment"] {
		t.Fatalf("comment %+v", c)
	}
	if st, _ := p.Status(); st.Session != SessionVerified || st.CheckedAt == "" || !p.Capabilities().Reply { // a post verifies the session
		t.Fatalf("status %+v", st)
	}
	// A body that already addresses someone is posted as is.
	if _, err := p.Reply(context.Background(), ItemExternalID(fileID, targetID), "@someone else"); err != nil {
		t.Fatal(err)
	}
	if got := f.postForms[1]["comment"]; got != "@someone else" {
		t.Fatalf("second comment %q", got)
	}
}

func TestReplyExpiredSession(t *testing.T) {
	for name, answer := range map[string]func(*http.Request) (int, string){
		"refused": func(*http.Request) (int, string) {
			return http.StatusOK, `{"success":false,"error":"You must be logged in to perform that action."}`
		},
		"unauthorized": func(*http.Request) (int, string) { return http.StatusUnauthorized, `` },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			p := f.provider(t)
			f.signedIn(t, p)
			f.postReply = answer
			_, err := p.Reply(context.Background(), ItemExternalID(fileID, targetID), "hi")
			if !errors.Is(err, ErrSessionExpired) || !errors.Is(err, provider.ErrNotSignedIn) {
				t.Fatalf("err %v", err)
			}
			if st, _ := p.Status(); st.Session != SessionExpired {
				t.Fatalf("session %q", st.Session)
			}
			// Expired: no further post until new cookies are pasted.
			if _, err := p.Reply(context.Background(), ItemExternalID(fileID, targetID), "hi"); !errors.Is(err, ErrSessionExpired) || len(f.posts) != 1 {
				t.Fatalf("second try err %v posts %d", err, len(f.posts))
			}
			f.signedIn(t, p)
			if st, _ := p.Status(); st.Session != SessionStored {
				t.Fatalf("after new cookies %q", st.Session)
			}
		})
	}
}

func TestReplyWithoutCookies(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	if _, err := p.Reply(context.Background(), ItemExternalID(fileID, targetID), "hi"); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("err %v", err)
	}
	if len(f.posts) != 0 || f.count("/comment/PublishedFile_Public/render/"+ownerID+"/"+fileID+"/") != 0 {
		t.Fatal("requests sent without cookies")
	}
	if _, err := p.Reply(context.Background(), "issue:1", "hi"); err == nil {
		t.Fatal("bad external id accepted")
	}
}

func TestReplyLostAnswerReadBack(t *testing.T) {
	for _, landed := range []bool{true, false} {
		t.Run(fmt.Sprint("landed=", landed), func(t *testing.T) {
			f := newFake(t)
			p := f.provider(t)
			p.opts.HTTP = &http.Client{Timeout: 150 * time.Millisecond}
			f.signedIn(t, p)
			f.postReply = func(r *http.Request) (int, string) {
				if landed {
					f.blocks = append([]string{f.ownerBlock("900000000000000002", r.PostForm.Get("comment"), time.Now())}, f.blocks...)
				}
				f.mu.Unlock() // let the read-back through while this answer is lost
				time.Sleep(400 * time.Millisecond)
				f.mu.Lock()
				return http.StatusOK, `{"success":true}`
			}
			c, err := p.Reply(context.Background(), ItemExternalID(fileID, targetID), "thanks")
			if landed {
				if err != nil || c.ExternalID != "900000000000000002" {
					t.Fatalf("read-back: %+v %v", c, err)
				}
				return
			}
			if !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("err %v, want outcome unknown", err)
			}
			if len(f.posts) != 1 {
				t.Fatalf("retried: %d posts", len(f.posts))
			}
		})
	}
}

// L1: a refusal of the old cookies must neither flag nor overwrite cookies
// pasted meanwhile (in memory and on disk).
func TestSessionFlagKeepsNewCookies(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	f.signedIn(t, p)
	old, err := p.current()
	if err != nil {
		t.Fatal(err)
	}
	ls2, sid2 := "76561197996210591%7C%7CeyJhbGciOi.new", "fedcba9876543210fedcba98"
	if _, err := p.Save(Update{LoginSecure: &ls2, SessionID: &sid2}); err != nil {
		t.Fatal(err)
	}
	_ = p.expire(old)
	if cur, _ := p.current(); cur.LoginSecure != ls2 || cur.SessionExpired {
		t.Fatalf("memory: %q expired=%v", cur.LoginSecure, cur.SessionExpired)
	}
	disk, err := loadSettings(p.opts.Dir)
	if err != nil || disk.LoginSecure != ls2 || disk.SessionExpired {
		t.Fatalf("disk: %q expired=%v %v", disk.LoginSecure, disk.SessionExpired, err)
	}
	cur, _ := p.current()
	_ = p.expire(cur)
	if c, _ := p.current(); !c.SessionExpired {
		t.Fatal("a refusal of the current cookies must flag them")
	}
}
