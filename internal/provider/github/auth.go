// Package github is the GitHub provider: GitHub App registration via the
// manifest flow, user sign-in (web flow + PKCE on a loopback redirect, device
// flow fallback) and issue sync over GraphQL/REST.
//
// Credentials live in the secrets dir (data\secrets): github-app.json (app id,
// client secret, private key) and github-token.json (user tokens). They are
// generated for this user at first run; nothing secret ships in the binary.
package github

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/secret"
)

const (
	appFile   = "github-app.json"
	tokenFile = "github-token.json" //nolint:gosec // G101: a file name, not a credential

	// CallbackPath receives the user authorization code.
	CallbackPath = "/auth/github/callback"
	// AppCreatedPath receives the manifest conversion code.
	AppCreatedPath = "/auth/github/app-created"
	// SetupPath is where GitHub sends the user after installing the app.
	SetupPath = "/auth/github/setup"

	homepage     = "https://github.com/UberMorgott/IssueWatcher"
	refreshAhead = 5 * time.Minute
)

// ErrNoApp means the GitHub App has not been registered yet.
var ErrNoApp = errors.New("github: app not registered")

// App is the per-user GitHub App created by the manifest flow.
type App struct {
	ID             int64     `json:"id"`
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	Owner          string    `json:"owner"`
	HTMLURL        string    `json:"htmlUrl"`
	ClientID       string    `json:"clientId"`
	ClientSecret   string    `json:"clientSecret"`
	WebhookSecret  string    `json:"webhookSecret,omitempty"`
	PEM            string    `json:"pem"`
	RegisteredPort int       `json:"registeredPort"` // loopback port baked into the callback URLs
	CreatedAt      time.Time `json:"createdAt"`
}

// Token is a GitHub App user access token pair.
type Token struct {
	AccessToken   string    `json:"accessToken"`
	RefreshToken  string    `json:"refreshToken,omitempty"`
	Expiry        time.Time `json:"expiry,omitzero"`        // zero = non-expiring
	RefreshExpiry time.Time `json:"refreshExpiry,omitzero"` // zero = unknown
	Login         string    `json:"login"`
	AvatarURL     string    `json:"avatarUrl,omitempty"`
}

// OAuthError is an error body from github.com/login/oauth/*.
type OAuthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *OAuthError) Error() string {
	return "github oauth: " + e.Code + ": " + e.Description
}

// Auth owns the app credentials, the user token and every GitHub sign-in step.
type Auth struct {
	WebURL string // https://github.com
	APIURL string // https://api.github.com
	HTTP   *http.Client
	Dir    string // secrets dir
	Now    func() time.Time
	// PollUnit scales the device-flow poll interval (seconds); tests shrink it.
	PollUnit time.Duration

	mu        sync.Mutex
	token     *Token     // cached; nil = not loaded or signed out
	gen       uint64     // bumped by Logout: a token request begun before it is dropped
	refreshMu sync.Mutex // one refresh at a time: a used refresh token dies
}

// NewAuth uses github.com and stores credentials in dir.
func NewAuth(dir string) *Auth {
	return &Auth{
		WebURL:   "https://github.com",
		APIURL:   "https://api.github.com",
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Dir:      dir,
		Now:      time.Now,
		PollUnit: time.Second,
	}
}

// App returns the registered app or ErrNoApp.
func (a *Auth) App() (App, error) {
	var app App
	err := secret.ReadJSON(filepath.Join(a.Dir, appFile), &app)
	if errors.Is(err, secret.ErrNotFound) {
		return app, ErrNoApp
	}
	return app, err
}

// PKCE returns a fresh S256 code verifier and its challenge (RFC 7636).
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// RandomState returns an unguessable OAuth state value.
func RandomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type manifest struct {
	Name                  string            `json:"name"`
	URL                   string            `json:"url"`
	Description           string            `json:"description"`
	HookAttributes        hookAttributes    `json:"hook_attributes"`
	RedirectURL           string            `json:"redirect_url"`
	CallbackURLs          []string          `json:"callback_urls"`
	SetupURL              string            `json:"setup_url"`
	Public                bool              `json:"public"`
	DefaultPermissions    map[string]string `json:"default_permissions"`
	DefaultEvents         []string          `json:"default_events"`
	RequestOAuthOnInstall bool              `json:"request_oauth_on_install"`
	SetupOnUpdate         bool              `json:"setup_on_update"`
}

type hookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// AppName is a globally unique app name for this installation.
func AppName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "IssueWatcher-" + hex.EncodeToString(b)
}

// Manifest is the GitHub App manifest for a loopback server at port.
// Callback URLs: the exact current port first, then the port-less loopback
// form that GitHub matches for any port (see docs/ARCHITECTURE.md → GitHub auth).
func Manifest(name string, port int) ([]byte, error) {
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	m := manifest{
		Name:        name,
		URL:         homepage,
		Description: "Personal IssueWatcher desktop hub (local only).",
		// Webhooks off: the app polls. hook_attributes.url is required when the
		// object is present, active=false means nothing is ever delivered.
		HookAttributes: hookAttributes{URL: homepage, Active: false},
		RedirectURL:    base + AppCreatedPath,
		CallbackURLs:   []string{base + CallbackPath, "http://127.0.0.1" + CallbackPath},
		SetupURL:       base + SetupPath,
		Public:         false,
		DefaultPermissions: map[string]string{
			"issues":        "write",
			"pull_requests": "write",
			"contents":      "write",
			"metadata":      "read",
		},
		DefaultEvents: []string{},
	}
	return json.Marshal(m)
}

// ManifestPostURL is where the browser POSTs the manifest (personal account).
func (a *Auth) ManifestPostURL(state string) string {
	return a.WebURL + "/settings/apps/new?state=" + url.QueryEscape(state)
}

// InstallURL is the page that installs app on the user's repositories.
func (a *Auth) InstallURL(app App) string {
	return a.WebURL + "/apps/" + url.PathEscape(app.Slug) + "/installations/new"
}

// ConvertManifest exchanges the manifest code for the app credentials and
// stores them. port is the loopback port the manifest was registered with.
func (a *Auth) ConvertManifest(ctx context.Context, code string, port int) (App, error) {
	var out struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		Name          string `json:"name"`
		HTMLURL       string `json:"html_url"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		PEM           string `json:"pem"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	u := a.APIURL + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return App{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if err := a.doJSON(req, http.StatusCreated, &out); err != nil {
		return App{}, fmt.Errorf("github: manifest conversion: %w", err)
	}
	if out.ClientID == "" || out.ClientSecret == "" || out.Slug == "" {
		return App{}, errors.New("github: manifest conversion: incomplete app credentials")
	}
	app := App{
		ID: out.ID, Slug: out.Slug, Name: out.Name, Owner: out.Owner.Login, HTMLURL: out.HTMLURL,
		ClientID: out.ClientID, ClientSecret: out.ClientSecret, WebhookSecret: out.WebhookSecret,
		PEM: out.PEM, RegisteredPort: port, CreatedAt: a.Now().UTC(),
	}
	if err := secret.WriteJSON(filepath.Join(a.Dir, appFile), app); err != nil {
		return App{}, err
	}
	return app, nil
}

// AuthorizeURL starts the web flow with state and a PKCE S256 challenge.
func (a *Auth) AuthorizeURL(app App, redirectURI, state, challenge string) string {
	q := url.Values{
		"client_id":             {app.ClientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return a.WebURL + "/login/oauth/authorize?" + q.Encode()
}

// Exchange trades the authorization code for tokens and stores them.
func (a *Auth) Exchange(ctx context.Context, code, verifier, redirectURI string) (Token, error) {
	app, err := a.App()
	if err != nil {
		return Token{}, err
	}
	return a.tokenRequest(ctx, url.Values{
		"client_id":     {app.ClientID},
		"client_secret": {app.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}, "", "")
}

// tokenRequest posts to the token endpoint, resolves the login, stores the token.
// login/avatar are reused when already known (refresh).
func (a *Auth) tokenRequest(ctx context.Context, form url.Values, login, avatar string) (Token, error) {
	var out struct {
		OAuthError
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	a.mu.Lock()
	gen := a.gen
	a.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.WebURL+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if err := a.doJSON(req, http.StatusOK, &out); err != nil {
		return Token{}, fmt.Errorf("github: token request: %w", err)
	}
	if out.Code != "" { // GitHub reports OAuth errors with 200 OK
		oe := out.OAuthError
		return Token{}, &oe
	}
	if out.AccessToken == "" {
		return Token{}, errors.New("github: token response without access_token")
	}
	now := a.Now().UTC()
	t := Token{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, Login: login, AvatarURL: avatar}
	if out.ExpiresIn > 0 {
		t.Expiry = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	if out.RefreshTokenExpiresIn > 0 {
		t.RefreshExpiry = now.Add(time.Duration(out.RefreshTokenExpiresIn) * time.Second)
	}
	if t.Login == "" {
		if t.Login, t.AvatarURL, err = a.userProfile(ctx, t.AccessToken); err != nil {
			return Token{}, err
		}
	}
	if err := a.saveToken(&t, gen); err != nil {
		return Token{}, err
	}
	return t, nil
}

func (a *Auth) userProfile(ctx context.Context, accessToken string) (login, avatar string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.APIURL+"/user", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	var u struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := a.doJSON(req, http.StatusOK, &u); err != nil {
		return "", "", fmt.Errorf("github: get user: %w", err)
	}
	return u.Login, u.AvatarURL, nil
}

// saveToken stores t unless Logout ran since the request began (gen): a late
// refresh or exchange must not sign the user back in.
func (a *Auth) saveToken(t *Token, gen uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gen != gen {
		return fmt.Errorf("%w: signed out while the token request ran", provider.ErrNotSignedIn)
	}
	if err := secret.WriteJSON(filepath.Join(a.Dir, tokenFile), t); err != nil {
		return err
	}
	a.token = t
	return nil
}

// loadToken returns the cached or stored token; caller holds a.mu.
func (a *Auth) loadToken() (*Token, error) {
	if a.token != nil {
		return a.token, nil
	}
	var t Token
	err := secret.ReadJSON(filepath.Join(a.Dir, tokenFile), &t)
	if errors.Is(err, secret.ErrNotFound) {
		return nil, provider.ErrNotSignedIn
	}
	if err != nil {
		return nil, err
	}
	a.token = &t
	return a.token, nil
}

// Login is the signed-in user, "" when signed out.
func (a *Auth) Login() string {
	login, _ := a.Profile()
	return login
}

// Profile is the signed-in user's login and avatar URL ("" when signed out).
func (a *Auth) Profile() (login, avatarURL string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, err := a.loadToken()
	if err != nil {
		return "", ""
	}
	return t.Login, t.AvatarURL
}

// AccessToken returns a valid user token, refreshing it shortly before expiry.
// ErrNotSignedIn when there is no token or the refresh token is dead.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	a.mu.Lock()
	t, err := a.loadToken()
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	now := a.Now()
	if t.Expiry.IsZero() || now.Add(refreshAhead).Before(t.Expiry) {
		return t.AccessToken, nil
	}
	if t.RefreshToken == "" || (!t.RefreshExpiry.IsZero() && now.After(t.RefreshExpiry)) {
		_ = a.Logout()
		return "", provider.ErrNotSignedIn
	}
	nt, err := a.refresh(ctx, t)
	if err != nil {
		if _, ok := errors.AsType[*OAuthError](err); ok { // bad_refresh_token etc.: must sign in again
			_ = a.Logout()
			return "", fmt.Errorf("%w: %w", provider.ErrNotSignedIn, err)
		}
		return "", err
	}
	return nt.AccessToken, nil
}

func (a *Auth) refresh(ctx context.Context, t *Token) (Token, error) {
	app, err := a.App()
	if err != nil {
		return Token{}, err
	}
	return a.tokenRequest(ctx, url.Values{
		"client_id":     {app.ClientID},
		"client_secret": {app.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
	}, t.Login, t.AvatarURL)
}

// Logout forgets the user token (the app registration stays).
func (a *Auth) Logout() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = nil
	a.gen++
	return secret.Remove(filepath.Join(a.Dir, tokenFile))
}

// DeviceCode is the device flow grant shown to the user.
type DeviceCode struct {
	DeviceCode      string `json:"-"`
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationUri"`
	ExpiresIn       int    `json:"expiresIn"`
	Interval        int    `json:"interval"`
}

// DeviceStart requests a device code (device flow must be enabled in the app settings).
func (a *Auth) DeviceStart(ctx context.Context) (DeviceCode, error) {
	app, err := a.App()
	if err != nil {
		return DeviceCode{}, err
	}
	var out struct {
		OAuthError
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.WebURL+"/login/device/code",
		strings.NewReader(url.Values{"client_id": {app.ClientID}}.Encode()))
	if err != nil {
		return DeviceCode{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if err := a.doJSON(req, http.StatusOK, &out); err != nil {
		return DeviceCode{}, fmt.Errorf("github: device code: %w", err)
	}
	if out.Code != "" {
		oe := out.OAuthError
		return DeviceCode{}, &oe
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	return DeviceCode{
		DeviceCode: out.DeviceCode, UserCode: out.UserCode, VerificationURI: out.VerificationURI,
		ExpiresIn: out.ExpiresIn, Interval: out.Interval,
	}, nil
}

// DevicePoll polls until the user approves, denies or the code expires.
func (a *Auth) DevicePoll(ctx context.Context, dc DeviceCode) (Token, error) {
	app, err := a.App()
	if err != nil {
		return Token{}, err
	}
	interval := time.Duration(dc.Interval) * a.PollUnit
	for {
		select {
		case <-ctx.Done():
			return Token{}, ctx.Err()
		case <-time.After(interval):
		}
		t, err := a.tokenRequest(ctx, url.Values{
			"client_id":   {app.ClientID},
			"device_code": {dc.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}, "", "")
		var oe *OAuthError
		switch {
		case err == nil:
			return t, nil
		case errors.As(err, &oe) && oe.Code == "authorization_pending":
		case errors.As(err, &oe) && oe.Code == "slow_down":
			interval += 5 * a.PollUnit
		default:
			return Token{}, err
		}
	}
}

// doJSON sends req, requires status want, decodes the body into out.
func (a *Auth) doJSON(req *http.Request, want int, out any) error {
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != want {
		return fmt.Errorf("unexpected status %s: %s", resp.Status, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
