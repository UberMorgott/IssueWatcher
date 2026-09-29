package steam

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Steam sign-in without typing anything into IssueWatcher (docs/ARCHITECTURE.md
// → Mod platforms › Sign-in): the dashboard shows a QR code from Steam's own
// authentication service (IAuthenticationService/BeginAuthSessionViaQR), the
// user scans it with the Steam mobile app and approves, the poll returns a
// refresh token, and login.steampowered.com/jwt/finalizelogin turns it into
// the steamcommunity.com web session (steamLoginSecure + sessionid). The same
// refresh token renews the web session when its access token expires, so the
// user signs in again only when the refresh token itself runs out. This is the
// flow of the steam-session library (EAuthTokenPlatformType.WebBrowser).

// QR login states (GET /api/platforms/steam/login).
const (
	QRNone    = ""        // no sign-in running
	QRPending = "pending" // QR shown, waiting for the scan
	QRScanned = "scanned" // scanned, waiting for the approval in the app
	QRDone    = "done"    // signed in; SteamID and cookies stored
	QRExpired = "expired" // the code timed out or was declined: start again
	QRFailed  = "failed"
)

// qrTimeout ends an unanswered sign-in (the app's code stays valid about as long).
const qrTimeout = 5 * time.Minute

// refreshMargin renews the web session this long before its access token expires.
const refreshMargin = 10 * time.Minute

// ErrRefreshExpired: Steam no longer accepts the stored refresh token (an ErrSessionExpired).
var ErrRefreshExpired = fmt.Errorf("%w (the sign-in itself ran out)", ErrSessionExpired)

// QRStatus is the progress of a QR sign-in.
type QRStatus struct {
	State        string `json:"state"`
	ChallengeURL string `json:"challengeUrl,omitempty"` // the QR code's content
	SteamID      string `json:"steamId,omitempty"`
	Error        string `json:"error,omitempty"`
}

type qrLogin struct {
	cancel context.CancelFunc
	status QRStatus
}

// StartQR begins a QR sign-in (a running one is cancelled) and polls it in
// the background until it is approved, declined or times out.
func (p *Provider) StartQR(ctx context.Context) (QRStatus, error) {
	var r struct {
		Response struct {
			ClientID     string  `json:"client_id"`
			ChallengeURL string  `json:"challenge_url"`
			RequestID    string  `json:"request_id"`
			Interval     float64 `json:"interval"`
		} `json:"response"`
	}
	form := url.Values{
		"device_friendly_name": {"IssueWatcher"},
		"platform_type":        {"2"}, // EAuthTokenPlatformType.WebBrowser: tokens for the web audience
		"website_id":           {"Community"},
	}
	if err := p.authCall(ctx, "BeginAuthSessionViaQR", form, &r); err != nil {
		return QRStatus{}, err
	}
	if r.Response.ClientID == "" || r.Response.RequestID == "" || r.Response.ChallengeURL == "" {
		return QRStatus{}, errors.New("steam: BeginAuthSessionViaQR: incomplete answer")
	}
	interval := time.Duration(r.Response.Interval * float64(time.Second))
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if p.opts.QRInterval > 0 {
		interval = p.opts.QRInterval
	}
	bg, cancel := context.WithTimeout(context.Background(), qrTimeout)
	l := &qrLogin{cancel: cancel, status: QRStatus{State: QRPending, ChallengeURL: r.Response.ChallengeURL}}
	p.qmu.Lock()
	if p.qr != nil {
		p.qr.cancel()
	}
	p.qr = l
	st := l.status
	p.qmu.Unlock()
	go p.pollQR(bg, l, r.Response.ClientID, r.Response.RequestID, interval)
	return st, nil
}

// QRLoginStatus reports the current (or last) QR sign-in.
func (p *Provider) QRLoginStatus() QRStatus {
	p.qmu.Lock()
	defer p.qmu.Unlock()
	if p.qr == nil {
		return QRStatus{State: QRNone}
	}
	return p.qr.status
}

// CancelQR stops a running QR sign-in.
func (p *Provider) CancelQR() {
	p.qmu.Lock()
	defer p.qmu.Unlock()
	if p.qr != nil {
		p.qr.cancel()
		p.qr = nil
	}
}

func (p *Provider) setQR(l *qrLogin, f func(*QRStatus)) {
	p.qmu.Lock()
	defer p.qmu.Unlock()
	f(&l.status)
}

func (p *Provider) pollQR(ctx context.Context, l *qrLogin, clientID, requestID string, interval time.Duration) {
	defer l.cancel()
	for {
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			p.setQR(l, func(s *QRStatus) {
				if s.State == QRPending || s.State == QRScanned {
					s.State, s.ChallengeURL = QRExpired, ""
				}
			})
			return
		case <-t.C:
		}
		var r struct {
			Response struct {
				RefreshToken    string `json:"refresh_token"`
				NewChallengeURL string `json:"new_challenge_url"`
				Interaction     bool   `json:"had_remote_interaction"`
			} `json:"response"`
		}
		err := p.authCall(ctx, "PollAuthSessionStatus", url.Values{"client_id": {clientID}, "request_id": {requestID}}, &r)
		var eres *eresultError
		switch {
		case errors.As(err, &eres): // the session is gone: timed out or declined in the app
			p.setQR(l, func(s *QRStatus) { s.State, s.ChallengeURL = QRExpired, "" })
			return
		case ctx.Err() != nil:
			continue // reported as expired above
		case err != nil:
			p.opts.Log.Warn("steam: QR poll failed, retrying", "err", err)
			continue
		}
		if r.Response.RefreshToken == "" {
			p.setQR(l, func(s *QRStatus) {
				if r.Response.NewChallengeURL != "" {
					s.ChallengeURL = r.Response.NewChallengeURL
				}
				if r.Response.Interaction {
					s.State = QRScanned
				}
			})
			continue
		}
		id, err := p.signIn(ctx, r.Response.RefreshToken)
		p.setQR(l, func(s *QRStatus) {
			s.ChallengeURL = ""
			if err != nil {
				s.State, s.Error = QRFailed, err.Error()
				return
			}
			s.State, s.SteamID = QRDone, id
		})
		if err != nil {
			p.opts.Log.Error("steam: QR sign-in failed", "err", err)
			return
		}
		p.opts.Log.Info("steam: signed in by QR", "steamId", id)
		if p.opts.OnSignIn != nil {
			p.opts.OnSignIn()
		}
		return
	}
}

// signIn turns a refresh token into the web session, checks it, and stores
// SteamID, cookies and the refresh token.
func (p *Provider) signIn(ctx context.Context, refresh string) (string, error) {
	sess, err := p.finalize(ctx, refresh)
	if err != nil {
		return "", err
	}
	if err := p.checkCookies(ctx, sess.steamID, sess.loginSecure, sess.sessionID); err != nil {
		return "", err
	}
	p.save.Lock()
	defer p.save.Unlock()
	cur, err := p.current()
	if err != nil {
		return "", err
	}
	next := cur
	next.SteamID, next.LoginSecure, next.SessionID, next.RefreshToken = sess.steamID, sess.loginSecure, sess.sessionID, refresh
	next.SessionExpired, next.Verified, next.CheckedAt = false, true, p.opts.Now().UTC()
	if err := saveSettings(p.opts.Dir, next); err != nil {
		return "", err
	}
	p.mu.Lock()
	p.settings = next
	if next.SteamID != cur.SteamID {
		p.creators = map[string]string{}
	}
	p.mu.Unlock()
	return sess.steamID, nil
}

type webSession struct{ steamID, loginSecure, sessionID string }

// finalize exchanges a refresh token for steamcommunity.com cookies:
// jwt/finalizelogin answers transfer targets, the community one sets
// steamLoginSecure.
func (p *Provider) finalize(ctx context.Context, refresh string) (webSession, error) {
	sid := make([]byte, 12)
	if _, err := rand.Read(sid); err != nil {
		return webSession{}, err
	}
	sessionID := hex.EncodeToString(sid)
	a, err := p.postMultipart(ctx, p.opts.LoginURL+"/jwt/finalizelogin", map[string]string{
		"nonce": refresh, "sessionid": sessionID, "redir": p.opts.CommunityURL + "/login/home/?goto=",
	})
	if err != nil {
		return webSession{}, err
	}
	var r struct {
		SteamID  string `json:"steamID"`
		Error    int    `json:"error"`
		Transfer []struct {
			URL    string         `json:"url"`
			Params map[string]any `json:"params"`
		} `json:"transfer_info"`
	}
	if a.Status != http.StatusOK || json.Unmarshal(a.Body, &r) != nil {
		return webSession{}, fmt.Errorf("steam: finalizelogin: HTTP %d", a.Status)
	}
	if r.Error != 0 || r.SteamID == "" {
		return webSession{}, fmt.Errorf("%w (finalizelogin error %d)", ErrRefreshExpired, r.Error)
	}
	if !steamID64Re.MatchString(r.SteamID) {
		return webSession{}, fmt.Errorf("steam: finalizelogin: bad steamID %q", r.SteamID)
	}
	for _, t := range r.Transfer {
		if !p.communityURL(t.URL) {
			continue
		}
		form := map[string]string{"steamID": r.SteamID}
		for k, v := range t.Params {
			form[k] = fmt.Sprint(v)
		}
		ta, err := p.postMultipart(ctx, t.URL, form)
		if err != nil {
			return webSession{}, err
		}
		for _, c := range (&http.Response{Header: ta.Header}).Cookies() {
			if c.Name == "steamLoginSecure" && c.Value != "" && tokenRe.MatchString(c.Value) {
				return webSession{steamID: r.SteamID, loginSecure: c.Value, sessionID: sessionID}, nil
			}
		}
		return webSession{}, fmt.Errorf("steam: settoken: HTTP %d without steamLoginSecure", ta.Status)
	}
	return webSession{}, errors.New("steam: finalizelogin: no steamcommunity.com transfer")
}

// communityURL: a transfer target on the community site (not store/help).
func (p *Provider) communityURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	c, _ := url.Parse(p.opts.CommunityURL)
	return (u.Scheme == "https" || c.Scheme == "http") && u.Host == c.Host
}

// checkCookies confirms the web session: /my/ goes to the member's own
// profile when signed in and to /login otherwise.
func (p *Provider) checkCookies(ctx context.Context, steamID, loginSecure, sessionID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.opts.CommunityURL+"/my/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", "sessionid="+sessionID+"; steamLoginSecure="+loginSecure)
	a, err := p.do(req)
	if err != nil {
		return err
	}
	path := ""
	if a.URL != nil {
		path = a.URL.Path
	}
	switch {
	case strings.Contains(path, "/login"):
		return ErrSessionExpired
	case a.Status == http.StatusOK && (strings.HasPrefix(path, "/profiles/"+steamID) || strings.HasPrefix(path, "/id/")):
		return nil
	default:
		return fmt.Errorf("steam: session check: HTTP %d at %s", a.Status, path)
	}
}

// CheckSession verifies stored cookies (renewing them first when due) and
// records the result: Reply is offered only for a verified session.
func (p *Provider) CheckSession(ctx context.Context) error {
	s, err := p.current()
	if err != nil {
		return err
	}
	if s.LoginSecure == "" || s.SessionID == "" {
		return fmt.Errorf("%w: not signed in to Steam", provider.ErrNotSignedIn)
	}
	s, err = p.ensureFresh(ctx, s)
	if err != nil {
		return err
	}
	err = p.checkCookies(ctx, s.SteamID, s.LoginSecure, s.SessionID)
	switch {
	case errors.Is(err, ErrSessionExpired):
		if s.RefreshToken != "" {
			if s2, rerr := p.refresh(ctx, s); rerr == nil {
				p.sessionOK(s2)
				return nil
			}
		}
		return p.expire(s)
	case err != nil:
		return err
	}
	p.sessionOK(s)
	return nil
}

// ensureFresh renews the web session when its access token is (nearly)
// expired and a refresh token is stored.
func (p *Provider) ensureFresh(ctx context.Context, s Settings) (Settings, error) {
	if s.RefreshToken == "" {
		return s, nil
	}
	if !s.SessionExpired && s.LoginSecure != "" {
		if exp, ok := accessExpiry(s.LoginSecure); !ok || exp.After(p.opts.Now().Add(refreshMargin)) {
			return s, nil
		}
	}
	return p.refresh(ctx, s)
}

// refresh gets new cookies from the refresh token; a refused token marks the
// session expired (the user signs in again).
func (p *Provider) refresh(ctx context.Context, seen Settings) (Settings, error) {
	sess, err := p.finalize(ctx, seen.RefreshToken)
	if errors.Is(err, ErrRefreshExpired) {
		p.setSession(seen, true)
		return seen, ErrRefreshExpired
	}
	if err != nil {
		return seen, err
	}
	p.save.Lock()
	defer p.save.Unlock()
	p.mu.Lock()
	s := p.settings
	if s.RefreshToken != seen.RefreshToken || s.SteamID != sess.steamID {
		p.mu.Unlock()
		return s, errors.New("steam: account changed during the session renewal")
	}
	s.LoginSecure, s.SessionID, s.SessionExpired, s.Verified, s.CheckedAt = sess.loginSecure, sess.sessionID, false, true, p.opts.Now().UTC()
	p.settings = s
	p.mu.Unlock()
	if err := saveSettings(p.opts.Dir, s); err != nil {
		return s, err
	}
	p.opts.Log.Info("steam: web session renewed")
	return s, nil
}

// accessExpiry reads exp of the access token inside steamLoginSecure
// ("<steamid>||<jwt>", URL-encoded).
func accessExpiry(loginSecure string) (time.Time, bool) {
	v, err := url.QueryUnescape(loginSecure)
	if err != nil {
		return time.Time{}, false
	}
	_, jwt, ok := strings.Cut(v, "||")
	if !ok {
		return time.Time{}, false
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(c.Exp, 0), true
}

// eresultError: an IAuthenticationService call answered an EResult other than OK.
type eresultError struct{ code int }

func (e *eresultError) Error() string { return "steam: auth service EResult " + strconv.Itoa(e.code) }

// authCall posts to IAuthenticationService/<method>/v1 (plain form fields,
// JSON answer; the EResult comes in the X-eresult header).
func (p *Provider) authCall(ctx context.Context, method string, form url.Values, out any) error {
	a, err := p.postForm(ctx, p.opts.APIURL+"/IAuthenticationService/"+method+"/v1/", form, "")
	if err != nil {
		return err
	}
	if e := a.Header.Get("X-eresult"); e != "" && e != "1" {
		code, _ := strconv.Atoi(e)
		return &eresultError{code: code}
	}
	if a.Status != http.StatusOK {
		return fmt.Errorf("steam: %s: HTTP %d", method, a.Status)
	}
	if err := json.Unmarshal(a.Body, out); err != nil {
		return fmt.Errorf("steam: %s: %w", method, err)
	}
	return nil
}

// postMultipart sends a multipart form as the site's own login pages do.
func (p *Provider) postMultipart(ctx context.Context, u string, fields map[string]string) (answer, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return answer{}, err
		}
	}
	if err := w.Close(); err != nil {
		return answer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", p.opts.CommunityURL)
	req.Header.Set("Referer", p.opts.CommunityURL+"/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	return p.do(req)
}
