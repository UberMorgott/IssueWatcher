package steam

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAuth is Steam's sign-in side: IAuthenticationService (QR), jwt/finalizelogin,
// the community settoken transfer and /my/ (session check), plus the comment
// post endpoint for the renewal tests.
type fakeAuth struct {
	t    *testing.T
	srv  *httptest.Server
	base string // the site URL the fake answers as (its own, or a combined test server)

	mu        sync.Mutex
	polls     int
	decline   bool   // PollAuthSessionStatus answers EResult 9 (declined / timed out)
	refresh   string // refresh token the poll hands out and finalizelogin accepts
	exp       time.Time
	issued    int      // cookies issued by settoken
	valid     []string // accepted steamLoginSecure values
	finalized int
	posts     []string // Cookie headers of comment posts
}

func newFakeAuth(t *testing.T) *fakeAuth {
	t.Helper()
	f := &fakeAuth{t: t, refresh: "refresh-token-1", exp: time.Now().Add(24 * time.Hour)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.base = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func fakeJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"EdDSA"}`)) + "." + enc(fmt.Appendf(nil, `{"sub":"%s","exp":%d}`, ownerID, exp.Unix())) + ".sig"
}

func (f *fakeAuth) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/IAuthenticationService/BeginAuthSessionViaQR/v1/":
		_ = r.ParseForm()
		if r.PostForm.Get("platform_type") != "2" || r.PostForm.Get("device_friendly_name") == "" {
			w.Header().Set("X-eresult", "8")
			writeJSON(w, map[string]any{"response": map[string]any{}})
			return
		}
		w.Header().Set("X-eresult", "1")
		writeJSON(w, map[string]any{"response": map[string]any{
			"client_id": "123", "challenge_url": "https://s.team/q/1/123", "request_id": "cmVxdWVzdA==", "interval": 5, "version": 1,
		}})
	case "/IAuthenticationService/PollAuthSessionStatus/v1/":
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != "123" || r.PostForm.Get("request_id") != "cmVxdWVzdA==" {
			w.Header().Set("X-eresult", "8")
			return
		}
		f.polls++
		if f.decline {
			w.Header().Set("X-eresult", "9")
			writeJSON(w, map[string]any{"response": map[string]any{}})
			return
		}
		w.Header().Set("X-eresult", "1")
		switch f.polls {
		case 1:
			writeJSON(w, map[string]any{"response": map[string]any{"new_challenge_url": "https://s.team/q/1/456", "had_remote_interaction": false}})
		case 2:
			writeJSON(w, map[string]any{"response": map[string]any{"had_remote_interaction": true}})
		default:
			writeJSON(w, map[string]any{"response": map[string]any{"refresh_token": f.refresh, "access_token": "a", "account_name": "owner"}})
		}
	case "/jwt/finalizelogin":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "multipart expected", http.StatusBadRequest)
			return
		}
		f.finalized++
		if r.FormValue("nonce") != f.refresh || len(r.FormValue("sessionid")) != 24 || r.Header.Get("Origin") != f.base {
			writeJSON(w, map[string]any{"error": 8})
			return
		}
		writeJSON(w, map[string]any{"steamID": ownerID, "redir": r.FormValue("redir"), "transfer_info": []map[string]any{
			{"url": "https://store.example.invalid/login/settoken", "params": map[string]any{"nonce": "x", "auth": "y"}},
			{"url": f.base + "/login/settoken", "params": map[string]any{"nonce": "n1", "auth": "a1"}},
		}})
	case "/login/settoken":
		if err := r.ParseMultipartForm(1 << 20); err != nil || r.FormValue("steamID") != ownerID || r.FormValue("nonce") != "n1" || r.FormValue("auth") != "a1" {
			http.Error(w, "bad transfer", http.StatusBadRequest)
			return
		}
		f.issued++
		v := ownerID + "%7C%7C" + fakeJWT(f.exp.Add(time.Duration(f.issued)*time.Second))
		f.valid = append(f.valid, v)
		http.SetCookie(w, &http.Cookie{Name: "steamLoginSecure", Value: v, Path: "/", Secure: true, HttpOnly: true})
		writeJSON(w, map[string]any{"result": 1})
	case "/my/":
		if f.accepts(r) {
			http.Redirect(w, r, "/profiles/"+ownerID+"/", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/login/home/?goto=%2Fmy%2F", http.StatusFound)
	case "/profiles/" + ownerID + "/", "/login/home/":
		_, _ = w.Write([]byte("<html></html>"))
	default:
		if strings.HasPrefix(r.URL.Path, "/comment/PublishedFile_Public/post/") {
			f.posts = append(f.posts, r.Header.Get("Cookie"))
			if !f.accepts(r) {
				writeJSON(w, map[string]any{"success": false, "error": "You must be logged in to perform that action."})
				return
			}
			writeJSON(w, map[string]any{"success": true, "comments_html": ""})
			return
		}
		http.NotFound(w, r)
	}
}

// accepts: the request carries a steamLoginSecure this fake issued last.
func (f *fakeAuth) accepts(r *http.Request) bool {
	c, err := r.Cookie("steamLoginSecure")
	return err == nil && len(f.valid) > 0 && c.Value == f.valid[len(f.valid)-1]
}

func (f *fakeAuth) provider(t *testing.T, dir string, onSignIn func()) *Provider {
	t.Helper()
	return New(Options{Dir: dir, CommunityURL: f.srv.URL, APIURL: f.srv.URL, LoginURL: f.srv.URL,
		MinGap: time.Millisecond, QRInterval: 5 * time.Millisecond, OnSignIn: onSignIn})
}

func waitQR(t *testing.T, p *Provider, want string) QRStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := p.QRLoginStatus(); st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("QR state %q, want %q", p.QRLoginStatus().State, want)
	return QRStatus{}
}

func TestQRSignIn(t *testing.T) {
	f := newFakeAuth(t)
	dir := t.TempDir()
	var signedIn atomic.Int32
	p := f.provider(t, dir, func() { signedIn.Add(1) })
	if p.Capabilities().Reply {
		t.Fatal("reply on before sign-in")
	}
	st, err := p.StartQR(context.Background())
	if err != nil || st.State != QRPending || st.ChallengeURL != "https://s.team/q/1/123" {
		t.Fatalf("start %+v %v", st, err)
	}
	done := waitQR(t, p, QRDone)
	if done.SteamID != ownerID || done.ChallengeURL != "" || signedIn.Load() != 1 {
		t.Fatalf("done %+v signIn %d", done, signedIn.Load())
	}
	s, err := p.Status()
	if err != nil || s.SteamID != ownerID || !s.HasCookies || s.Session != SessionVerified || !s.SignedIn {
		t.Fatalf("status %+v %v", s, err)
	}
	if !p.Capabilities().Reply {
		t.Fatal("reply off after a verified sign-in")
	}
	// Stored (DPAPI): a new provider on the same folder has the session.
	q := f.provider(t, dir, nil)
	if acc, err := q.Account(context.Background()); err != nil || acc != ownerID || !q.Capabilities().Reply {
		t.Fatalf("reloaded account %q %v", acc, err)
	}
}

func TestQRDeclinedExpires(t *testing.T) {
	f := newFakeAuth(t)
	f.decline = true
	p := f.provider(t, t.TempDir(), nil)
	if _, err := p.StartQR(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitQR(t, p, QRExpired)
	if s, _ := p.Status(); s.Configured || s.HasCookies {
		t.Fatalf("declined sign-in stored something: %+v", s)
	}
}

func TestQRCancel(t *testing.T) {
	f := newFakeAuth(t)
	p := New(Options{Dir: t.TempDir(), CommunityURL: f.srv.URL, APIURL: f.srv.URL, LoginURL: f.srv.URL, MinGap: time.Millisecond, QRInterval: time.Hour})
	if _, err := p.StartQR(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.CancelQR()
	if st := p.QRLoginStatus(); st.State != QRNone {
		t.Fatalf("after cancel %+v", st)
	}
}

// signInQR runs a whole QR sign-in on p.
func signInQR(t *testing.T, p *Provider) {
	t.Helper()
	if _, err := p.StartQR(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitQR(t, p, QRDone)
}

func TestExpiredAccessTokenRenewedBeforeCheck(t *testing.T) {
	f := newFakeAuth(t)
	f.exp = time.Now().Add(-time.Hour) // every issued access token is already expired
	p := f.provider(t, t.TempDir(), nil)
	signInQR(t, p)
	before := f.finalized
	if err := p.CheckSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.finalized != before+1 {
		t.Fatalf("finalizelogin calls %d → %d, want one renewal", before, f.finalized)
	}
	if s, _ := p.Status(); s.Session != SessionVerified {
		t.Fatalf("session %q", s.Session)
	}
}

func TestRefusedCookiesRenewedOnce(t *testing.T) {
	f := newFakeAuth(t)
	p := f.provider(t, t.TempDir(), nil)
	signInQR(t, p)
	f.mu.Lock()
	f.valid = append(f.valid, "revoked") // Steam dropped the web session; the refresh token still works
	f.mu.Unlock()
	if err := p.CheckSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s, _ := p.Status(); s.Session != SessionVerified || !p.Capabilities().Reply {
		t.Fatalf("after renewal %+v", s)
	}
}

func TestRefreshRefusedNeedsSignIn(t *testing.T) {
	f := newFakeAuth(t)
	p := f.provider(t, t.TempDir(), nil)
	signInQR(t, p)
	f.mu.Lock()
	f.valid = append(f.valid, "revoked")
	f.refresh = "another-token" // the stored refresh token is refused too
	f.mu.Unlock()
	err := p.CheckSession(context.Background())
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err %v", err)
	}
	if s, _ := p.Status(); s.Session != SessionExpired || p.Capabilities().Reply {
		t.Fatalf("status %+v", s)
	}
}

func TestPastedCookiesAreNotTheQRSession(t *testing.T) {
	f := newFakeAuth(t)
	p := f.provider(t, t.TempDir(), nil)
	signInQR(t, p)
	ls, sid := ownerID+"%7C%7Cpasted", "0123456789abcdef01234567"
	st, err := p.Save(Update{LoginSecure: &ls, SessionID: &sid})
	if err != nil || st.SignedIn || st.Session != SessionStored || p.Capabilities().Reply {
		t.Fatalf("pasted %+v %v", st, err)
	}
}

func TestAccessExpiry(t *testing.T) {
	at := time.Unix(1893456000, 0)
	if got, ok := accessExpiry(ownerID + "%7C%7C" + fakeJWT(at)); !ok || !got.Equal(at) {
		t.Fatalf("expiry %v %v", got, ok)
	}
	for _, bad := range []string{"", "x", ownerID + "%7C%7Cnot.a.jwt", ownerID + "||a.b"} {
		if _, ok := accessExpiry(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestReplyRenewsRefusedSessionAndPostsOnce(t *testing.T) {
	st := newFake(t)
	a := newFakeAuth(t)
	auth := map[string]bool{
		"/IAuthenticationService/BeginAuthSessionViaQR/v1/": true, "/IAuthenticationService/PollAuthSessionStatus/v1/": true,
		"/jwt/finalizelogin": true, "/login/settoken": true, "/my/": true, "/profiles/" + ownerID + "/": true, "/login/home/": true,
	}
	mux := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth[r.URL.Path] {
			a.serve(w, r)
			return
		}
		st.serve(w, r)
	}))
	t.Cleanup(mux.Close)
	a.base = mux.URL
	st.acceptPosts()
	accept := st.postReply
	st.postReply = func(r *http.Request) (int, string) {
		a.mu.Lock()
		ok := a.accepts(r)
		a.mu.Unlock()
		if !ok {
			return http.StatusOK, `{"success":false,"error":"You must be logged in to perform that action."}`
		}
		return accept(r)
	}
	p := New(Options{Dir: t.TempDir(), CommunityURL: mux.URL, APIURL: mux.URL, LoginURL: mux.URL,
		MinGap: time.Millisecond, PageSize: 10, QRInterval: 5 * time.Millisecond})
	signInQR(t, p)
	a.mu.Lock()
	a.valid = append(a.valid, "revoked") // the web session was dropped; the refresh token still works
	a.mu.Unlock()
	c, err := p.Reply(context.Background(), ItemExternalID(fileID, targetID), "renewed")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.posts) != 2 || c.Body != "@user1 renewed" {
		t.Fatalf("posts %d body %q", len(st.posts), c.Body)
	}
	if s, _ := p.Status(); s.Session != SessionVerified {
		t.Fatalf("session %q", s.Session)
	}
}
