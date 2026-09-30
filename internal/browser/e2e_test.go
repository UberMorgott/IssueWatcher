package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRealBrowser drives the installed browser against a local https page:
// IW_BROWSER_E2E=1 (IW_BROWSER_EXE picks another exe, e.g. Cent Browser).
func TestRealBrowser(t *testing.T) {
	if os.Getenv("IW_BROWSER_E2E") != "1" {
		t.Skip("set IW_BROWSER_E2E=1")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "s3cret", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(time.Hour)})
			_, _ = w.Write([]byte("<html><title>home</title><body>ok</body></html>"))
		case "/api":
			c, err := r.Cookie("sid")
			if err != nil || r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				http.Error(w, "no cookie", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte("hello " + c.Value + " " + r.UserAgent()))
		}
	}))
	defer srv.Close()
	origin := srv.URL
	find := Find
	if exe := os.Getenv("IW_BROWSER_EXE"); exe != "" {
		find = func() (Exe, error) { return Exe{Path: exe, Name: "custom"}, nil }
	}
	b := New(Options{Dir: t.TempDir(), Find: find, Origins: []string{origin}, Args: []string{"--ignore-certificate-errors"}})
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	r, err := b.Fetch(ctx, Request{URL: origin + "/api", Headers: map[string]string{"X-Requested-With": "XMLHttpRequest"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: first fetch %v", b.Name(), time.Since(start))
	if r.Status != 200 || !strings.HasPrefix(r.Body, "hello s3cret") || strings.Contains(r.Body, "HeadlessChrome") {
		t.Fatalf("fetch = %d %q", r.Status, r.Body)
	}
	host := strings.TrimPrefix(origin, "https://")
	host, _, _ = strings.Cut(host, ":")
	cks, err := b.Cookies(ctx, []string{host})
	if err != nil || len(cks) != 1 || cks[0].Value != "s3cret" {
		t.Fatalf("cookies = %+v, %v", cks, err)
	}
	if _, err := b.Fetch(ctx, Request{URL: "https://example.com/"}); err == nil {
		t.Fatal("fetch outside the allowlist worked")
	}
	if os.Getenv("IW_BROWSER_HEADED") == "1" {
		if err := b.OpenWindow(ctx, origin+"/"); err != nil {
			t.Fatal(err)
		}
		if !b.Headed() || !b.WindowOpen(ctx) {
			t.Fatal("no visible window")
		}
		// Same profile: the cookie survives the switch.
		cks, err = b.Cookies(ctx, []string{host})
		if err != nil || len(cks) != 1 {
			t.Fatalf("headed cookies = %+v, %v", cks, err)
		}
		b.Stop()
		r, err = b.Fetch(ctx, Request{URL: origin + "/api", Headers: map[string]string{"X-Requested-With": "XMLHttpRequest"}})
		if err != nil || r.Status != 200 || b.Headed() {
			t.Fatalf("back to headless: %d %v", r.Status, err)
		}
	}
	if err := b.ClearSite(ctx, []string{host}, []string{origin}); err != nil {
		t.Fatal(err)
	}
	if cks, _ := b.Cookies(ctx, []string{host}); len(cks) != 0 {
		t.Fatalf("cookies after ClearSite: %+v", cks)
	}
}

// TestLiveNexusWidget is the read-only live probe: IW_BROWSER_LIVE=1.
func TestLiveNexusWidget(t *testing.T) {
	if os.Getenv("IW_BROWSER_LIVE") != "1" {
		t.Skip("set IW_BROWSER_LIVE=1")
	}
	b := New(Options{Dir: t.TempDir(), Origins: []string{"https://www.nexusmods.com"}})
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	u := "https://www.nexusmods.com/Core/Libs/Common/Widgets/CommentContainer?RH_CommentContainer=game_id:8851,object_id:147,object_type:1,thread_id:16796543,tabbed:1,skip_opening_post:0,page:1"
	r, err := b.Fetch(ctx, Request{URL: u, Page: "https://www.nexusmods.com/windrose/mods/147", Headers: map[string]string{"X-Requested-With": "XMLHttpRequest"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d, %d bytes, %d comments, %v", b.Name(), r.Status, len(r.Body), strings.Count(r.Body, `class="comment `), time.Since(start))
	if r.Status != 200 || !strings.Contains(r.Body, "comment-content-text") {
		t.Fatalf("widget = %d %.300q", r.Status, r.Body)
	}
}
