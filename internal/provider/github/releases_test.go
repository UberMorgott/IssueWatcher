package github

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github/githubtest"
)

func TestReleaseCreateGetUpload(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	signIn(t, gh, a)
	p := NewProvider(a)
	ctx := t.Context()

	if _, found, err := p.GetReleaseByTag(ctx, "octo/app", "v1.2.3"); err != nil || found {
		t.Fatalf("absent release: found %v err %v", found, err)
	}
	rel, err := p.CreateRelease(ctx, "octo/app", provider.NewRelease{TagName: "v1.2.3", TargetCommitish: "abc123",
		Name: "v1.2.3", Body: "- fix"})
	if err != nil || rel.ID == 0 || rel.TagName != "v1.2.3" || !strings.HasSuffix(rel.UploadURL, "/assets{?name,label}") ||
		!strings.HasSuffix(rel.HTMLURL, "/octo/app/releases/tag/v1.2.3") {
		t.Fatalf("create %+v %v", rel, err)
	}
	gh.Mu.Lock()
	got := *gh.Releases[0]
	gh.Mu.Unlock()
	if got.TargetCommitish != "abc123" || got.Body != "- fix" || got.Draft || got.Prerelease {
		t.Fatalf("create body %+v", got)
	}

	data := bytes.Repeat([]byte("zipdata\x00\x01"), 4096)
	path := filepath.Join(t.TempDir(), "mod_1.2.3.zip")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	as, err := p.UploadReleaseAsset(ctx, rel, "mod_1.2.3.zip", path, "application/zip")
	if err != nil || as.ID == 0 || as.Name != "mod_1.2.3.zip" || as.Size != int64(len(data)) || as.State != "uploaded" ||
		!strings.HasPrefix(as.Digest, "sha256:") || as.BrowserDownloadURL == "" {
		t.Fatalf("upload %+v %v", as, err)
	}
	gh.Mu.Lock()
	up := gh.Releases[0].Assets[0]
	gh.Mu.Unlock()
	if !bytes.Equal(up.Data, data) || up.ContentType != "application/zip" {
		t.Fatalf("uploaded body %d bytes, type %q", len(up.Data), up.ContentType)
	}
	if _, err := p.UploadReleaseAsset(ctx, rel, "mod_1.2.3.zip", path, ""); err == nil {
		t.Fatal("duplicate asset name accepted")
	}

	back, found, err := p.GetReleaseByTag(ctx, "octo/app", "v1.2.3")
	if err != nil || !found || back.ID != rel.ID || len(back.Assets) != 1 {
		t.Fatalf("get %+v %v %v", back, found, err)
	}
	if byName, ok := back.Asset("mod_1.2.3.zip"); !ok || byName != as {
		t.Fatalf("asset by name %+v, want %+v", byName, as)
	}

	var paths []string
	for _, c := range gh.Calls() {
		if strings.Contains(c.Path, "/releases") {
			paths = append(paths, c.Method+" "+c.Path)
		}
	}
	want := []string{"GET /repos/octo/app/releases/tags/v1.2.3", "POST /repos/octo/app/releases",
		"POST /repos/octo/app/releases/1001/assets", "POST /repos/octo/app/releases/1001/assets",
		"GET /repos/octo/app/releases/tags/v1.2.3"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v\nwant %v", paths, want)
	}
}

// A failure other than 404 is an error, not "absent".
func TestGetReleaseByTagErrors(t *testing.T) {
	gh := githubtest.New(t)
	a := newAuth(t, gh)
	signIn(t, gh, a)
	p := NewProvider(a)
	if _, _, err := p.GetReleaseByTag(t.Context(), "bad", "v1"); err == nil {
		t.Fatal("bad repo id accepted")
	}
	gh.RevokeAccess()
	if _, found, err := p.GetReleaseByTag(t.Context(), "octo/app", "v1"); err == nil || found {
		t.Fatalf("401: found %v err %v", found, err)
	}
}

func TestAssetUploadURL(t *testing.T) {
	u, err := assetUploadURL(provider.Release{UploadURL: "https://uploads.github.com/repos/o/r/releases/7/assets{?name,label}"}, "a b.zip")
	if err != nil || u != "https://uploads.github.com/repos/o/r/releases/7/assets?name=a+b.zip" {
		t.Fatalf("%q %v", u, err)
	}
	if _, err := assetUploadURL(provider.Release{UploadURL: "assets{?name}"}, "x"); err == nil {
		t.Fatal("relative upload_url accepted")
	}
}
