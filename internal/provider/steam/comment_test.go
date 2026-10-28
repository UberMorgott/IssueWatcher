package steam

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

func TestPostCommentDryRunPostsNothing(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	f.signedIn(t, p)
	r, err := p.PostComment(context.Background(), fileID, "  New shields!\n", true)
	if err != nil {
		t.Fatal(err)
	}
	if want := p.opts.CommunityURL + "/comment/PublishedFile_Public/post/" + ownerID + "/" + fileID + "/"; r.Endpoint != want {
		t.Fatalf("endpoint %q, want %q", r.Endpoint, want)
	}
	if r.Creator != ownerID || r.Body != "New shields!" || r.BodyLength != 12 || !r.DryRun || !r.SignedIn || r.Comment != nil ||
		r.URL != p.fileURL(fileID) {
		t.Fatalf("plan %+v", r)
	}
	if len(f.posts) != 0 {
		t.Fatalf("dry run posted %d", len(f.posts))
	}
}

func TestPostCommentTopLevelForm(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	f.signedIn(t, p)
	f.acceptPosts()
	r, err := p.PostComment(context.Background(), fileID, "Shields v2 is out.", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.posts) != 1 || f.posts[0].URL.Path != "/comment/PublishedFile_Public/post/"+ownerID+"/"+fileID+"/" {
		t.Fatalf("posts %d", len(f.posts))
	}
	want := map[string]string{ // no @author: a top-level comment
		"comment": "Shields v2 is out.", "count": "10",
		"sessionid": "0123456789abcdef01234567", "feature2": "-1",
	}
	if got := f.postForms[0]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("form %v, want %v", got, want)
	}
	if ck, err := f.posts[0].Cookie("steamLoginSecure"); err != nil || ck.Value != "76561197996210591%7C%7CeyJhbGciOi.test" {
		t.Fatalf("steamLoginSecure %v %v", ck, err)
	}
	if r.Comment == nil || r.Comment.ExternalID != "900000000000000001" || r.URL != p.fileURL(fileID)+"#comment_900000000000000001" {
		t.Fatalf("result %+v", r)
	}
}

func TestPostCommentRefusals(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	if _, err := p.PostComment(context.Background(), fileID, "hi", false); !errors.Is(err, provider.ErrNotSignedIn) {
		t.Fatalf("no cookies: %v", err)
	}
	if _, err := p.PostComment(context.Background(), fileID, strings.Repeat("x", MaxReply+1), false); !errors.Is(err, provider.ErrReplyTooLong) {
		t.Fatalf("too long: %v", err)
	}
	if _, err := p.PostComment(context.Background(), "12a", "hi", true); err == nil {
		t.Fatal("bad item id accepted")
	}
	if _, err := p.PostComment(context.Background(), fileID, "  ", true); err == nil {
		t.Fatal("empty body accepted")
	}
	if len(f.posts) != 0 {
		t.Fatalf("posted %d", len(f.posts))
	}
}
