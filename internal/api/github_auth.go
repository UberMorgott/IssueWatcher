package api

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider/github"
)

// GitHub sign-in (docs/ARCHITECTURE.md → GitHub auth):
//
//	GET  /api/auth/status              {providers:[...]} + legacy GitHub fields
//	POST /api/auth/{provider}/start    opens the browser at: local manifest page (first run) or GitHub authorize URL; → {step,url}
//	POST /api/auth/{provider}/logout   forget the local token
//	POST /api/auth/github/device       device-flow fallback → {userCode, verificationUri}
//	GET  /auth/github/manifest  auto-submits the app manifest to github.com (session required)
//	GET  /auth/github/app-created, /setup, /callback  GitHub redirects; public, state-checked
//
// The three redirect targets skip the session check: they are top-level
// navigations from github.com, so the SameSite=Strict cookie is not sent.
// They act only on a single-use state we issued (setup only starts a new
// authorize round trip, which itself needs our state to complete).

const (
	stateManifest = "manifest"
	stateLogin    = "login"
	manifestTTL   = time.Hour // GitHub: finish the manifest flow within one hour
	loginTTL      = 10 * time.Minute
)

type oauthState struct {
	kind     string
	verifier string
	expires  time.Time
}

type deviceState struct {
	Pending         bool   `json:"pending"`
	UserCode        string `json:"userCode,omitempty"`
	VerificationURI string `json:"verificationUri,omitempty"`
	Error           string `json:"error,omitempty"`
}

type githubAuth struct {
	mu     sync.Mutex
	states map[string]oauthState
	device deviceState
	cancel context.CancelFunc // stops a running device poll
	gen    int                // device poll generation; stale polls do not touch device
	err    string             // last sign-in failure, cleared on start/success
}

var publicPaths = map[string]bool{
	github.AppCreatedPath: true,
	github.SetupPath:      true,
	github.CallbackPath:   true,
}

func (s *Server) registerGitHub(mux *http.ServeMux) {
	s.gh = &githubAuth{states: map[string]oauthState{}}
	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/{provider}/start", s.handleProviderStart)
	mux.HandleFunc("POST /api/auth/{provider}/logout", s.handleProviderLogout)
	mux.HandleFunc("POST /api/auth/github/device", s.handleDeviceStart)
	// Legacy aliases (github only; start returns the URL without opening it).
	mux.HandleFunc("POST /api/auth/start", s.handleAuthStart)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("POST /api/auth/device", s.handleDeviceStart)
	mux.HandleFunc("GET /auth/github/manifest", s.handleManifestPage)
	mux.HandleFunc("GET "+github.AppCreatedPath, s.handleAppCreated)
	mux.HandleFunc("GET "+github.SetupPath, s.handleSetup)
	mux.HandleFunc("GET "+github.CallbackPath, s.handleCallback)
}

func (g *githubAuth) newState(kind, verifier string, ttl time.Duration) string {
	st := github.RandomState()
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, v := range g.states {
		if now.After(v.expires) {
			delete(g.states, k)
		}
	}
	g.states[st] = oauthState{kind: kind, verifier: verifier, expires: now.Add(ttl)}
	return st
}

// takeState returns and (if consume) deletes a live state of kind.
func (g *githubAuth) takeState(st, kind string, consume bool) (oauthState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.states[st]
	if !ok || v.kind != kind || time.Now().After(v.expires) {
		return v, false
	}
	if consume {
		delete(g.states, st)
	}
	return v, true
}

func (s *Server) callbackURL() string { return s.BaseURL() + github.CallbackPath }

// authorizeURL starts a web-flow round trip with fresh state + PKCE.
func (s *Server) authorizeURL(app github.App) string {
	verifier, challenge := github.PKCE()
	st := s.gh.newState(stateLogin, verifier, loginTTL)
	return s.opts.GitHub.AuthorizeURL(app, s.callbackURL(), st, challenge)
}

// Provider connection states (GET /api/auth/status → providers[].state).
const (
	connNotConfigured = "not_configured" // GitHub: no app registered yet (first "Войти" creates it)
	connDisconnected  = "disconnected"
	connConnecting    = "connecting" // a browser round trip or device poll is in progress
	connConnected     = "connected"
	connError         = "error"
	connUnavailable   = "unavailable" // provider not implemented yet
)

// providerStatus is one entry of GET /api/auth/status → providers.
type providerStatus struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Connected   bool   `json:"connected"`
	Login       string `json:"login,omitempty"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
	SetupNeeded bool   `json:"setupNeeded"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
}

// futureProviders are listed so the UI can show them; they cannot connect yet.
var futureProviders = []providerStatus{
	{ID: "curseforge", Name: "CurseForge", State: connUnavailable},
	{ID: "nexus", Name: "Nexus Mods", State: connUnavailable},
	{ID: "steam", Name: "Steam Workshop", State: connUnavailable},
}

// authStatus is GET /api/auth/status: the providers list plus the legacy
// GitHub-only top-level fields.
type authStatus struct {
	Providers  []providerStatus `json:"providers"`
	App        bool             `json:"app"`
	AppSlug    string           `json:"appSlug,omitempty"`
	AppURL     string           `json:"appUrl,omitempty"`
	InstallURL string           `json:"installUrl,omitempty"`
	SignedIn   bool             `json:"signedIn"`
	Login      string           `json:"login,omitempty"`
	Device     deviceState      `json:"device"`
}

func (s *Server) authStatus() authStatus {
	var st authStatus
	if app, err := s.opts.GitHub.App(); err == nil {
		st.App, st.AppSlug, st.AppURL, st.InstallURL = true, app.Slug, app.HTMLURL, s.opts.GitHub.InstallURL(app)
	}
	login, avatar := s.opts.GitHub.Profile()
	st.Login, st.SignedIn = login, login != ""

	s.gh.mu.Lock()
	st.Device = s.gh.device
	lastErr := s.gh.err
	inFlight := false
	for _, v := range s.gh.states {
		inFlight = inFlight || time.Now().Before(v.expires)
	}
	s.gh.mu.Unlock()

	gh := providerStatus{ID: "github", Name: "GitHub", Connected: st.SignedIn, Login: login, AvatarURL: avatar, SetupNeeded: !st.App}
	switch {
	case st.SignedIn:
		gh.State = connConnected
	case st.Device.Pending || inFlight:
		gh.State = connConnecting
	case lastErr != "" || st.Device.Error != "":
		gh.State, gh.Error = connError, lastErr+st.Device.Error
	case !st.App:
		gh.State = connNotConfigured
	default:
		gh.State = connDisconnected
	}
	st.Providers = append([]providerStatus{gh}, futureProviders...)
	return st
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.authStatus())
}

// authChanged tells open dashboard tabs to reload the connection state.
func (s *Server) authChanged() {
	st := s.authStatus()
	s.Publish(EventAuthChanged, map[string]any{"provider": "github", "state": st.Providers[0].State, "login": st.Login})
}

func (s *Server) setAuthError(msg string) {
	s.gh.mu.Lock()
	s.gh.err = msg
	s.gh.mu.Unlock()
	s.authChanged()
}

// startURL begins GitHub sign-in: the local manifest page on first run,
// otherwise the GitHub authorize URL. local reports a same-origin path.
func (s *Server) startURL() (step, u string, local bool, err error) {
	s.gh.mu.Lock()
	s.gh.err = ""
	s.gh.mu.Unlock()
	app, err := s.opts.GitHub.App()
	switch {
	case errors.Is(err, github.ErrNoApp):
		st := s.gh.newState(stateManifest, "", manifestTTL)
		return "create_app", "/auth/github/manifest?state=" + st, true, nil
	case err != nil:
		return "", "", false, err
	default:
		return "authorize", s.authorizeURL(app), false, nil
	}
}

// handleProviderStart is POST /api/auth/{provider}/start: returns {step, url}
// and opens it in the default browser itself.
func (s *Server) handleProviderStart(w http.ResponseWriter, r *http.Request) {
	if !s.knownProvider(w, r) {
		return
	}
	step, u, local, err := s.startURL()
	if err != nil {
		s.opts.Log.Error("auth start: read app", "err", err)
		errJSON(w, http.StatusInternalServerError, "cannot read GitHub app credentials")
		return
	}
	open := u
	if local { // a fresh browser window has no session cookie yet
		open = s.LaunchURL(u)
	}
	s.opts.Open(open)
	s.authChanged()
	writeJSON(w, http.StatusOK, map[string]any{"step": step, "url": u, "opened": true})
}

// handleAuthStart is the legacy POST /api/auth/start: the page navigates itself.
func (s *Server) handleAuthStart(w http.ResponseWriter, _ *http.Request) {
	step, u, _, err := s.startURL()
	if err != nil {
		s.opts.Log.Error("auth start: read app", "err", err)
		errJSON(w, http.StatusInternalServerError, "cannot read GitHub app credentials")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"step": step, "url": u})
}

// knownProvider accepts github; future providers answer 501, others 404.
func (s *Server) knownProvider(w http.ResponseWriter, r *http.Request) bool {
	id := r.PathValue("provider")
	if id == "github" {
		return true
	}
	for _, p := range futureProviders {
		if p.ID == id {
			errJSON(w, http.StatusNotImplemented, p.Name+" is not supported yet")
			return false
		}
	}
	errJSON(w, http.StatusNotFound, "unknown provider")
	return false
}

func (s *Server) handleProviderLogout(w http.ResponseWriter, r *http.Request) {
	if s.knownProvider(w, r) {
		s.handleAuthLogout(w, r)
	}
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, _ *http.Request) {
	s.stopDevice()
	s.gh.mu.Lock()
	s.gh.states = map[string]oauthState{} // abandon pending round trips
	s.gh.err = ""
	s.gh.mu.Unlock()
	if err := s.opts.GitHub.Logout(); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.opts.Log.Info("github: signed out")
	s.authChanged()
	writeJSON(w, http.StatusOK, s.authStatus())
}

var manifestPage = template.Must(template.New("m").Parse(`<!doctype html>
<html lang="ru"><meta charset="utf-8"><title>IssueWatcher — создание GitHub App</title>
<body style="font-family:system-ui;margin:2rem">
<p>Создаём ваш приватный GitHub App для IssueWatcher на github.com…</p>
<form id="f" method="post" action="{{.Action}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<noscript><button type="submit">Перейти на GitHub</button></noscript>
</form>
<script>document.getElementById('f').submit()</script>
</body>`))

func (s *Server) handleManifestPage(w http.ResponseWriter, r *http.Request) {
	st := r.URL.Query().Get("state")
	if _, ok := s.gh.takeState(st, stateManifest, false); !ok {
		authPage(w, http.StatusForbidden, "Ссылка для входа устарела. Нажмите «Подключить GitHub» в панели ещё раз.")
		return
	}
	m, err := github.Manifest(github.AppName(), s.port)
	if err != nil {
		authPage(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = manifestPage.Execute(w, map[string]string{"Action": s.opts.GitHub.ManifestPostURL(st), "Manifest": string(m)})
}

func (s *Server) handleAppCreated(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, ok := s.gh.takeState(q.Get("state"), stateManifest, true); !ok {
		authPage(w, http.StatusForbidden, "Неизвестный или устаревший запрос. Нажмите «Подключить GitHub» в панели ещё раз.")
		return
	}
	app, err := s.opts.GitHub.ConvertManifest(r.Context(), q.Get("code"), s.port)
	if err != nil {
		s.opts.Log.Error("github: manifest conversion failed", "err", err)
		s.setAuthError("не удалось зарегистрировать GitHub App")
		authPage(w, http.StatusBadGateway, "GitHub не вернул данные приложения. Попробуйте «Подключить GitHub» ещё раз.")
		return
	}
	s.opts.Log.Info("github: app registered", "app_id", app.ID, "slug", app.Slug, "port", app.RegisteredPort)
	s.authChanged()
	http.Redirect(w, r, s.opts.GitHub.InstallURL(app), http.StatusSeeOther)
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	app, err := s.opts.GitHub.App()
	if err != nil {
		authPage(w, http.StatusConflict, "У IssueWatcher ещё нет GitHub App. Нажмите «Подключить GitHub» в панели.")
		return
	}
	s.opts.Log.Info("github: app installed", "setup_action", r.URL.Query().Get("setup_action"))
	http.Redirect(w, r, s.authorizeURL(app), http.StatusSeeOther)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	st, ok := s.gh.takeState(q.Get("state"), stateLogin, true)
	if !ok {
		authPage(w, http.StatusForbidden, "Неизвестный или устаревший вход. Нажмите «Подключить GitHub» в панели ещё раз.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.setAuthError("вход отменён на GitHub (" + e + ")")
		authPage(w, http.StatusForbidden, "Вход в GitHub отменён ("+e+").")
		return
	}
	tok, err := s.opts.GitHub.Exchange(r.Context(), q.Get("code"), st.verifier, s.callbackURL())
	if err != nil {
		s.opts.Log.Error("github: token exchange failed", "err", err)
		s.setAuthError("не удалось получить токен GitHub")
		authPage(w, http.StatusBadGateway, "Не удалось войти в GitHub. Попробуйте «Подключить GitHub» ещё раз.")
		return
	}
	s.signedIn(tok.Login)
	if s.navigate("") { // a dashboard tab is open: focus it, this tab is done
		authPage(w, http.StatusOK, "Вход в GitHub выполнен как "+tok.Login+". Эту вкладку можно закрыть.")
		return
	}
	// A fresh launch URL: the session cookie is not sent on this cross-site redirect chain.
	http.Redirect(w, r, s.LaunchURL("/"), http.StatusSeeOther)
}

func (s *Server) signedIn(login string) {
	s.opts.Log.Info("github: signed in", "login", login)
	s.gh.mu.Lock()
	s.gh.err = ""
	s.gh.mu.Unlock()
	s.authChanged()
	if s.opts.Sync != nil {
		s.opts.Sync.Trigger()
	}
}

// handleDeviceStart is the fallback when the loopback redirect cannot work:
// it needs "Enable Device Flow" ticked in the app settings.
func (s *Server) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	dc, err := s.opts.GitHub.DeviceStart(r.Context())
	if err != nil {
		var oe *github.OAuthError
		msg := err.Error()
		if errors.As(err, &oe) && oe.Code == "device_flow_disabled" {
			msg = "Device Flow выключен: включите его в настройках GitHub App"
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
		return
	}
	s.stopDevice()
	// The poll outlives this request but not the code's lifetime.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Duration(dc.ExpiresIn)*time.Second)
	s.gh.mu.Lock()
	s.gh.gen++
	gen := s.gh.gen
	s.gh.cancel = cancel
	s.gh.device = deviceState{Pending: true, UserCode: dc.UserCode, VerificationURI: dc.VerificationURI}
	s.gh.err = ""
	s.gh.mu.Unlock()
	s.authChanged()
	go func() {
		defer cancel()
		tok, err := s.opts.GitHub.DevicePoll(ctx, dc)
		s.gh.mu.Lock()
		if gen == s.gh.gen {
			s.gh.device = deviceState{}
			if err != nil && !errors.Is(err, context.Canceled) {
				s.gh.device.Error = err.Error()
			}
		}
		s.gh.mu.Unlock()
		if err == nil {
			s.signedIn(tok.Login)
		} else {
			s.authChanged()
		}
	}()
	s.opts.Open(dc.VerificationURI)
	writeJSON(w, http.StatusOK, dc)
}

func (s *Server) stopDevice() {
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	if s.gh.cancel != nil {
		s.gh.cancel()
		s.gh.cancel = nil
	}
}

var messagePage = template.Must(template.New("p").Parse(`<!doctype html>
<html lang="ru"><meta charset="utf-8"><title>IssueWatcher</title>
<body style="font-family:system-ui;margin:2rem"><p>{{.}}</p></body>`))

func authPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = messagePage.Execute(w, msg)
}
