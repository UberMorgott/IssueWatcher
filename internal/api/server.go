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
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

const (
	sessionCookie = "iw_session"
	launchTTL     = 2 * time.Minute
)

// Options configures the server.
type Options struct {
	Assets        fs.FS // built frontend (index.html at root)
	PreferredPort int   // used only when free; 0 = OS-chosen
	Version       string
	Open          func(url string) // opens a URL in the user's browser
	Log           *slog.Logger

	// Phase 1 (all optional; nil disables the matching endpoints).
	GitHub         *github.Auth   // sign-in endpoints
	Store          *store.Store   // data endpoints (need Sync too)
	Sync           *syncer.Syncer // poller status, sync now, replies
	OnUnreadChange func()         // called after the user marks an item read
}

// Server serves the SPA and the loopback API.
type Server struct {
	opts    Options
	ln      net.Listener
	srv     *http.Server
	port    int
	token   string // bearer for API clients; published in runtime.json
	session string // cookie value; memory only
	index   []byte

	mu       sync.Mutex
	launches map[string]launch

	gh  *githubAuth // nil without Options.GitHub
	hub *hub        // live events for open tabs (events.go)
}

type launch struct {
	next    string
	expires time.Time
}

// New binds the listener. Nothing is served until Serve.
func New(ctx context.Context, opts Options) (*Server, error) {
	ln, err := listen(ctx, opts.PreferredPort)
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
		session:  randomHex(32),
		launches: map[string]launch{},
		hub:      newHub(),
	}
	s.index, _ = fs.ReadFile(opts.Assets, "index.html") // nil → 503 "frontend not built"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth", s.handleAuth)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/open", s.handleOpen)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	if opts.GitHub != nil {
		s.registerGitHub(mux)
	}
	if opts.Store != nil && opts.Sync != nil {
		s.registerData(mux)
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

// listen prefers the configured port and falls back to an OS-chosen free one.
func listen(ctx context.Context, preferred int) (net.Listener, error) {
	var lc net.ListenConfig
	if preferred > 0 {
		if ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(preferred))); err == nil {
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

// OpenBrowser shows the dashboard at route next ("" = keep the current page):
// an open tab is steered there over SSE; only without one does a new browser
// tab open (via a one-time launch URL).
func (s *Server) OpenBrowser(next string) {
	if s.navigate(next) {
		return
	}
	s.opts.Open(s.LaunchURL(next))
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
			http.Error(w, "Not signed in. Open IssueWatcher from its tray icon.", http.StatusUnauthorized)
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
		http.Error(w, "Link expired. Open IssueWatcher from its tray icon.", http.StatusForbidden)
		return
	}
	// No Secure flag: plain-HTTP loopback, a Secure cookie would never be sent back.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure impossible on http://127.0.0.1; HttpOnly+SameSite=Strict set
		Name:     sessionCookie,
		Value:    s.session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
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
