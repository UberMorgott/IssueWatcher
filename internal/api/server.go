// Package api is the loopback HTTP server: the embedded Vue app plus a small
// JSON API, bound to 127.0.0.1 on an OS-chosen port and closed to anyone
// without the per-run secret.
//
// Auth: the browser is always opened through a one-time launch URL
// (/auth?t=...) that swaps the token for a session cookie; programmatic
// clients (second instance, later the MCP bridge) send the per-run bearer
// token from data\runtime.json.
package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

const (
	sessionCookie = "iw_session"
	sessionMaxAge = 400 * 24 * time.Hour // Chrome's cap for cookie lifetime
	launchTTL     = 2 * time.Minute
)

// Options configures the server.
type Options struct {
	Assets        fs.FS // built frontend (index.html at root)
	PreferredPort int   // used only when free; 0 = OS-chosen
	// RequirePort: PreferredPort or nothing (a restart after a self-update,
	// so open tabs reconnect to the same origin); New fails with ErrPortBusy.
	RequirePort bool
	Version     string
	Open        func(url string) // opens a URL in the user's browser
	Log         *slog.Logger

	// Phase 1 (all optional; nil disables the matching endpoints).
	GitHub         *github.Auth  // sign-in endpoints
	Store          *store.Store  // data endpoints (need Sync too)
	Sync           *syncer.Group // per-source syncers: status, sync now, replies
	OnUnreadChange func()        // called after the user marks an item read
	Settings       SettingsStore // GET/PATCH /api/settings (+ folders with Store)
	// TestNotification shows a sample popup (Settings → Notifications).
	TestNotification func()
	// Focus brings the browser window showing the dashboard to the front
	// (desktop shell, internal/notify); nil = SSE navigate only.
	Focus func() (title string, ok bool)
	// SessionSecret is the browser session cookie value kept across runs
	// (data\secrets\session.json), so open tabs and the installed app window
	// stay signed in after a restart; "" = a new one per run. The bearer
	// token for programmatic clients still changes every run.
	SessionSecret string
	// Updates is the self-updater (GET /api/update, check, install); nil disables it.
	Updates Updater
	// Runner is the agent job queue (/api/jobs, /api/agents); nil disables it (needs Store).
	Runner *runner.Runner
	// Picker shows the native folder dialog (FolderDialog «Обзор…»); nil (headless) → 409 unavailable.
	Picker FolderPicker
	// Steam is the Steam provider's account settings (/api/providers/steam); nil disables them.
	Steam SteamSettings
	// Platforms reports and checks the platform accounts (/api/platforms); nil disables them.
	Platforms Platforms
}

// Server serves the SPA and the loopback API.
type Server struct {
	opts    Options
	ln      net.Listener
	srv     *http.Server
	port    int
	token   string // bearer for API clients; published in runtime.json
	session string // cookie value (Options.SessionSecret or per run)
	index   []byte

	mu       sync.Mutex
	launches map[string]launch

	gh     *githubAuth // nil without Options.GitHub
	hub    *hub        // live events for open tabs (events.go)
	opener opener      // OpenBrowser decisions and the pending new tab (open.go)
	pickMu sync.Mutex  // one native folder dialog at a time (dialog.go)
}

type launch struct {
	next    string
	expires time.Time
}

// New binds the listener. Nothing is served until Serve.
func New(ctx context.Context, opts Options) (*Server, error) {
	ln, err := listen(ctx, opts.PreferredPort, opts.RequirePort)
	if err != nil {
		return nil, err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("api: unexpected listener address %v", ln.Addr())
	}
	s := &Server{
		opts:     opts,
		ln:       ln,
		port:     addr.Port,
		token:    randomHex(32),
		session:  cmp.Or(opts.SessionSecret, randomHex(32)),
		launches: map[string]launch{},
		hub:      newHub(),
	}
	s.index, _ = fs.ReadFile(opts.Assets, "index.html") // nil → 503 "frontend not built"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth", s.handleAuth)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/open", s.handleOpen)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	s.registerDialog(mux)
	if opts.GitHub != nil {
		s.registerGitHub(mux)
	}
	if opts.Store != nil && opts.Sync != nil {
		s.registerData(mux)
		s.registerLinks(mux)
		opts.Sync.OnProgress(s.syncProgress)
	}
	if opts.Settings != nil {
		s.registerSettings(mux)
		if opts.Store != nil {
			s.registerFolders(mux)
		}
	}
	if opts.Updates != nil {
		s.registerUpdate(mux)
	}
	if opts.Runner != nil && opts.Store != nil {
		s.registerJobs(mux)
	}
	if opts.Steam != nil {
		s.registerSteam(mux)
	}
	if opts.Platforms != nil {
		s.registerPlatforms(mux)
	}
	if opts.TestNotification != nil {
		mux.HandleFunc("POST /api/notifications/test", s.handleTestNotification)
	}
	mux.Handle("/api/", http.NotFoundHandler())
	mux.Handle("/", s.spa())

	s.srv = &http.Server{
		Handler:           s.guard(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.srv.RegisterOnShutdown(s.hub.closeAll)
	return s, nil
}

// ErrPortBusy: RequirePort was set and the port stayed taken.
var ErrPortBusy = errors.New("api: the app's port is taken by another program")

// requireWait is how long a required port may stay busy (the previous process
// may still be closing it).
var requireWait = 5 * time.Second

// listen prefers the configured port and falls back to an OS-chosen free one;
// with require it retries the preferred port for requireWait, then fails.
func listen(ctx context.Context, preferred int, require bool) (net.Listener, error) {
	var lc net.ListenConfig
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(preferred))
	if require {
		if preferred <= 0 {
			return nil, fmt.Errorf("api: no port to take over: %w", ErrPortBusy)
		}
		deadline := time.Now().Add(requireWait)
		for {
			ln, err := lc.Listen(ctx, "tcp", addr)
			if err == nil {
				return ln, nil
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("%w: 127.0.0.1:%d (%w)", ErrPortBusy, preferred, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if preferred > 0 {
		if ln, err := lc.Listen(ctx, "tcp", addr); err == nil {
			return ln, nil
		}
	}
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("api: listen: %w", err)
	}
	return ln, nil
}

// Port is the bound TCP port.
func (s *Server) Port() int { return s.port }

// Token is the per-run bearer token for API clients.
func (s *Server) Token() string { return s.token }

// BaseURL is http://127.0.0.1:<port>.
func (s *Server) BaseURL() string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
}

// Serve blocks until Shutdown.
func (s *Server) Serve() error {
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("api: serve: %w", err)
	}
	return nil
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// LaunchURL returns a one-time URL that signs the browser in and redirects to next.
func (s *Server) LaunchURL(next string) string {
	t := randomHex(16)
	s.mu.Lock()
	now := time.Now()
	for k, l := range s.launches { // drop expired
		if now.After(l.expires) {
			delete(s.launches, k)
		}
	}
	s.launches[t] = launch{next: safeNext(next), expires: now.Add(launchTTL)}
	s.mu.Unlock()
	return s.BaseURL() + "/auth?t=" + t
}

// guard rejects foreign Host headers (DNS rebinding) and unauthenticated requests.
func (s *Server) guard(next http.Handler) http.Handler {
	port := strconv.Itoa(s.port)
	allowedHosts := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !allowedHosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		public := r.URL.Path == "/auth" || (s.gh != nil && publicPaths[r.URL.Path])
		if !public && !s.authorized(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			http.Error(w, "Вход не выполнен. Откройте IssueWatcher через значок в трее.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if c, err := r.Cookie(sessionCookie); err == nil && equal(c.Value, s.session) {
		return true
	}
	if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && equal(b, s.token) {
		return true
	}
	return false
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("t")
	s.mu.Lock()
	l, ok := s.launches[t]
	delete(s.launches, t) // single use
	s.mu.Unlock()
	if !ok || time.Now().After(l.expires) {
		http.Error(w, "Ссылка устарела. Откройте IssueWatcher через значок в трее.", http.StatusForbidden)
		return
	}
	// No Secure flag: plain-HTTP loopback, a Secure cookie would never be sent back.
	c := &http.Cookie{ //nolint:gosec // G124: Secure impossible on http://127.0.0.1; HttpOnly+SameSite=Strict set
		Name:     sessionCookie,
		Value:    s.session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
	if s.opts.SessionSecret != "" {
		// Persistent secret: the cookie outlives the browser session too, so an
		// installed app window stays signed in across app and browser restarts.
		c.MaxAge = int(sessionMaxAge / time.Second)
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, l.next, http.StatusSeeOther)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": s.opts.Version, "port": s.port})
}

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	s.opts.Log.Info("api: open requested", "path", req.Path)
	s.OpenBrowser(req.Path)
	w.WriteHeader(http.StatusNoContent)
}

// spa serves files from the bundle and falls back to index.html for client routes.
func (s *Server) spa() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(s.opts.Assets, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") { // Vite content-hashed names
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				if path.Ext(p) == ".webmanifest" { // not in Go's built-in MIME table
					w.Header().Set("Content-Type", "application/manifest+json")
				}
				http.ServeFileFS(w, r, s.opts.Assets, p)
				return
			}
			if path.Ext(p) != "" { // missing file, not a client route
				http.NotFound(w, r)
				return
			}
		}
		if s.index == nil {
			http.Error(w, "frontend not built (run npm run build in frontend/)", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(s.index)
	})
}

// safeNext keeps redirects on this origin.
func safeNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	return next
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b)
}
