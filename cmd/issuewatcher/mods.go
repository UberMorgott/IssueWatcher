package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/browser"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/factorio"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/secret"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

// checkTimeout bounds one «Проверить» (an MCP server may start a browser first).
const checkTimeout = 90 * time.Second

// modPlatform is one running mod platform: over its MCP server (bridge set)
// or native (session set: the built-in engine's stored sign-in).
type modPlatform struct {
	bridge  *mcpbridge.Client
	session *signin.Manager
	prov    provider.Provider
	syncer  *syncer.Syncer
	cfg     config.ModPlatform
}

func (mp *modPlatform) native() bool { return mp != nil && mp.bridge == nil }

// modIDs are the switchable mod platforms (Steam has its own settings).
var modIDs = []string{nexus.Platform, curseforge.Platform, factorio.Platform}

// nativeEngine reports whether id runs the built-in engine under cfg.
func nativeEngine(id string, cfg config.ModPlatform) bool {
	return id == factorio.Platform || cfg.Engine == config.EngineNative
}

// modPlatforms runs the MCP-backed mod platforms switched on in settings
// (providers.<id>.enabled) and reports every platform's state for Settings ›
// Платформы. A switch, a new server command or author applies live: the
// platform's syncer is added to or removed from the group, its server child
// restarted on the next call.
type modPlatforms struct {
	cfgs     *config.Store
	st       *store.Store
	log      *slog.Logger
	group    *syncer.Group
	gh       provider.Provider
	steam    *steam.Provider
	onUpdate func([]store.Event, int)
	dataDir  string

	// native replaces building a native provider (tests: fake sites).
	native func(id string, author func() string) (provider.Provider, *signin.Manager)

	brMu sync.Mutex
	br   *browser.Browser // shared by the native engines, started lazily

	// onRelogin shows the tray card «<platform>: войдите снова» (set by main).
	onRelogin func(id, name string)
	// dial replaces the server command (tests: in-memory MCP servers).
	dial func(id string) func(ctx context.Context) (mcp.Transport, error)

	mu        sync.Mutex
	live      map[string]*modPlatform
	checks    map[string]api.PlatformStatus // last «Проверить» result per platform
	notified  map[string]bool               // relogin card shown since the platform was last connected
	lastLogin map[string]string             // last logged sign-in state per platform
	sessions  map[string][2]string          // MCP platforms: session source + browser, learnt after a sync
}

func newModPlatforms(cfgs *config.Store, st *store.Store, log *slog.Logger, group *syncer.Group,
	gh provider.Provider, stm *steam.Provider, onUpdate func([]store.Event, int), dataDir string,
) *modPlatforms {
	m := &modPlatforms{cfgs: cfgs, st: st, log: log, group: group, gh: gh, steam: stm, onUpdate: onUpdate, dataDir: dataDir,
		live: map[string]*modPlatform{}, checks: map[string]api.PlatformStatus{}, notified: map[string]bool{}}
	m.apply(cfgs.Get())
	group.OnProgress(func(p syncer.Progress) {
		if p.State == syncer.ProgressDone || p.State == syncer.ProgressError {
			go m.watchRelogin(context.Background())
			go m.refreshSessions(context.Background())
		}
	})
	stm.OnSignIn(func() { go m.connected(context.Background(), steam.Platform) })
	return m
}

// Close stops the MCP server children and the browser for good.
func (m *modPlatforms) Close() {
	m.mu.Lock()
	bridges := make([]*mcpbridge.Client, 0, len(m.live))
	var sessions []*signin.Manager
	for _, p := range m.live {
		if p.bridge != nil {
			bridges = append(bridges, p.bridge)
		}
		if p.session != nil {
			sessions = append(sessions, p.session)
		}
	}
	m.mu.Unlock()
	for _, b := range bridges {
		b.Close()
	}
	for _, s := range sessions {
		s.Cancel()
	}
	m.brMu.Lock()
	br := m.br
	m.brMu.Unlock()
	if br != nil {
		br.Close()
	}
}

func modConfig(p config.Providers, id string) config.ModPlatform {
	switch id {
	case nexus.Platform:
		return p.Nexus
	case factorio.Platform:
		return config.ModPlatform{Enabled: p.Factorio.Enabled, Author: p.Factorio.Author, Engine: config.EngineNative}
	}
	return p.CurseForge
}

// browser is the shared installed-browser driver of the native engines
// (profile data\browser, owner-only).
func (m *modPlatforms) browser() *browser.Browser {
	m.brMu.Lock()
	defer m.brMu.Unlock()
	if m.br == nil {
		dir := filepath.Join(m.dataDir, "browser")
		if err := secret.RestrictDir(dir); err != nil {
			m.log.Warn("browser profile dir", "err", err)
		}
		origins := slices.Concat(nexus.Origins, nexus.SignInOrigins, curseforge.Origins, factorio.Origins)
		m.br = browser.New(browser.Options{Dir: dir, Origins: origins, Log: m.log})
	}
	return m.br
}

// buildNative builds a platform's native provider and its sign-in.
func (m *modPlatforms) buildNative(id string, author func() string) (provider.Provider, *signin.Manager) {
	if m.native != nil {
		return m.native(id, author)
	}
	jar, err := websession.Open(filepath.Join(m.dataDir, "secrets", id+".json"))
	if err != nil {
		m.log.Warn("stored session unreadable: sign in again", "platform", id, "err", err)
	}
	br := m.browser()
	switch id {
	case nexus.Platform:
		mgr := signin.New(nexus.SignInSpec(nil, m.log), jar, br)
		return nexus.New(nexus.Options{Native: &nexus.NativeOptions{Browser: br, Session: mgr}, Author: author, Log: m.log}), mgr
	case factorio.Platform:
		mgr := signin.New(factorio.SignInSpec(nil, m.log), jar, br)
		return factorio.New(factorio.Options{Session: mgr, Author: author, Log: m.log}), mgr
	}
	mgr := signin.New(curseforge.SignInSpec(nil, m.log), jar, br)
	return curseforge.New(curseforge.Options{Native: &curseforge.NativeOptions{Browser: br, Session: mgr}, Author: author, Log: m.log}), mgr
}

// apply brings the running platforms in line with the settings. Bridges are
// stopped after m.mu is released: a stop may wait for an in-flight call, and
// Platforms (GET /api/platforms) must not wait behind it.
func (m *modPlatforms) apply(cfg config.Settings) {
	var closing, stopping []*mcpbridge.Client
	var cancelling []*signin.Manager
	defer func() {
		for _, b := range closing {
			b.Close()
		}
		for _, b := range stopping {
			b.Stop()
		}
		for _, s := range cancelling {
			s.Cancel()
		}
	}()
	drop := func(p *modPlatform) {
		if p.bridge != nil {
			closing = append(closing, p.bridge) // final: a late call must not start an unowned server
		}
		if p.session != nil {
			cancelling = append(cancelling, p.session)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range modIDs {
		want := modConfig(cfg.Providers, id)
		cur := m.live[id]
		switch {
		case want.Enabled && cur == nil:
			p := m.start(id, cfg)
			m.live[id] = p
			m.group.Add(p.syncer)
			m.log.Info("mod platform on", "platform", id, "native", p.native())
		case !want.Enabled && cur != nil:
			m.group.Remove(id)
			drop(cur)
			delete(m.live, id)
			delete(m.checks, id)
			delete(m.sessions, id)
			m.log.Info("mod platform off", "platform", id)
		case cur != nil && nativeEngine(id, want) != cur.native():
			// The engine changed: a new provider (same platform id, same source).
			m.group.Remove(id)
			drop(cur)
			p := m.start(id, cfg)
			m.live[id] = p
			m.group.Add(p.syncer)
			delete(m.checks, id)
			delete(m.sessions, id)
			m.log.Info("mod platform engine switched", "platform", id, "native", p.native())
		case cur != nil && cur.bridge != nil && (want.MCP.Command != cur.cfg.MCP.Command || !slices.Equal(want.MCP.Args, cur.cfg.MCP.Args)):
			stopping = append(stopping, cur.bridge) // the next call starts the new command
			cur.cfg = want
			delete(m.checks, id)
			cur.syncer.Trigger()
		case cur != nil && want.Author != cur.cfg.Author:
			cur.cfg = want
			delete(m.checks, id)
			cur.syncer.Trigger()
		}
	}
}

// start builds a platform's provider (native, or over its MCP bridge) and
// syncer. m.mu must be held.
func (m *modPlatforms) start(id string, cfg config.Settings) *modPlatform {
	pick := func() config.ModPlatform { return modConfig(m.cfgs.Get().Providers, id) }
	if nativeEngine(id, modConfig(cfg.Providers, id)) {
		p, mgr := m.buildNative(id, func() string { return pick().Author })
		s := syncer.New(syncer.Options{Store: m.st, Provider: p, Plan: syncPlan(cfg.Sync), Log: m.log, OnUpdate: m.onUpdate})
		return &modPlatform{session: mgr, prov: p, syncer: s, cfg: modConfig(cfg.Providers, id)}
	}
	opts := mcpbridge.Options{Name: id, Log: m.log, Command: func() (string, []string) {
		s := pick().MCP
		return os.ExpandEnv(s.Command), s.Args
	}}
	if m.dial != nil {
		opts.Dial = m.dial(id)
	}
	b := mcpbridge.New(opts)
	author := func() string { return pick().Author }
	var p provider.Provider
	if id == nexus.Platform {
		p = nexus.New(nexus.Options{Bridge: b, Log: m.log, Author: author})
	} else {
		p = curseforge.New(curseforge.Options{Bridge: b, Log: m.log, Author: author})
	}
	s := syncer.New(syncer.Options{Store: m.st, Provider: p, Plan: syncPlan(cfg.Sync), Log: m.log, OnUpdate: m.onUpdate})
	return &modPlatform{bridge: b, prov: p, syncer: s, cfg: modConfig(cfg.Providers, id)}
}

var platformNames = map[string]string{
	"github": "GitHub", nexus.Platform: "Nexus Mods", curseforge.Platform: "CurseForge", factorio.Platform: "Factorio Mod Portal",
	steam.Platform: "Steam Workshop",
}

// Platforms implements api.Platforms: every platform, GitHub first.
func (m *modPlatforms) Platforms(ctx context.Context) []api.PlatformStatus {
	sources := map[string]syncer.SourceStatus{}
	for _, s := range m.group.Status().Sources {
		sources[s.Platform] = s
	}
	projects, err := m.st.ProjectCounts(ctx)
	if err != nil {
		projects = map[string]int{}
	}
	cfg := m.cfgs.Get().Providers
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.PlatformStatus, 0, 5)
	for _, id := range []string{"github", nexus.Platform, curseforge.Platform, factorio.Platform, steam.Platform} {
		ps := api.PlatformStatus{ID: id, Name: platformNames[id], Enabled: true, Projects: projects[id]}
		steamExpired := false
		switch id {
		case "github":
			ps.Capabilities = m.gh.Capabilities()
		case steam.Platform:
			ps.Capabilities = m.steam.Capabilities()
			st, err := m.steam.Status()
			ps.Enabled = err == nil && st.Configured
			steamExpired = err == nil && st.Session == steam.SessionExpired
			ps.Session, ps.AccountName = steamSession(st), st.Persona
		default:
			ps.Enabled = modConfig(cfg, id).Enabled
			switch p := m.live[id]; {
			case p != nil:
				ps.Capabilities = p.prov.Capabilities()
				if p.bridge != nil {
					ps.Running = p.bridge.Running()
				}
			case id == nexus.Platform:
				ps.Capabilities = nexus.New(nexus.Options{}).Capabilities()
			case id == factorio.Platform:
				ps.Capabilities = factorio.New(factorio.Options{}).Capabilities()
			default:
				ps.Capabilities = curseforge.New(curseforge.Options{}).Capabilities()
			}
		}
		if ps.Capabilities.Kinds == nil {
			ps.Capabilities.Kinds = []string{}
		}
		src, synced := sources[id]
		switch {
		case !ps.Enabled:
			ps.State = api.PlatformDisabled
		case steamExpired:
			ps.State, ps.Error = api.PlatformRelogin, steam.ErrSessionExpired.Error()
		case synced && src.Relogin:
			ps.State, ps.Error = api.PlatformRelogin, src.LastError
		case m.checks[id].State != "":
			c := m.checks[id]
			ps.State, ps.Account, ps.Error, ps.CheckedAt = c.State, c.Account, c.Error, c.CheckedAt
			if id != steam.Platform {
				ps.Session, ps.Browser = c.Session, c.Browser
			}
		case synced && src.Account != "" && src.LastError == "" && m.sessions[id][0] != "":
			ps.State, ps.Account = api.PlatformConnected, src.Account
			ps.Session, ps.Browser = m.sessions[id][0], m.sessions[id][1]
		case synced && src.Account != "" && src.LastError == "":
			ps.State, ps.Account = api.PlatformConnected, src.Account
		case synced && src.LastError != "":
			ps.State, ps.Error = api.PlatformError, src.LastError
		default:
			ps.State = api.PlatformUnknown
		}
		if synced {
			ps.LastSync = src.LastSync
		}
		out = append(out, ps)
	}
	return out
}

// Check implements api.Platforms: a live account check of one platform.
func (m *modPlatforms) Check(ctx context.Context, id string) (api.PlatformStatus, error) {
	var p provider.Provider
	m.mu.Lock()
	switch id {
	case steam.Platform:
		p = m.steam
	case nexus.Platform, curseforge.Platform, factorio.Platform:
		if mp := m.live[id]; mp != nil {
			p = mp.prov
		}
	}
	m.mu.Unlock()
	if id != steam.Platform && !slices.Contains(modIDs, id) {
		return api.PlatformStatus{}, api.ErrUnknownPlatform
	}
	res := api.PlatformStatus{ID: id, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if p == nil {
		res.State = api.PlatformDisabled
		return m.finish(ctx, id, res), nil
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var err error
	if id == steam.Platform {
		// Steam's account is just the saved SteamID: read the Workshop list
		// instead, and check the web session (it enables replies) when stored.
		if res.Account, err = p.Account(ctx); err == nil {
			_, err = p.ListProjects(ctx)
		}
		if st, serr := m.steam.Status(); err == nil && serr == nil && st.HasCookies {
			err = m.steam.CheckSession(ctx)
		}
		if err == nil {
			if _, perr := m.steam.RefreshPersona(ctx); perr != nil {
				m.log.Debug("steam: profile name lookup failed", "err", perr)
			}
		}
	} else {
		res.Account, err = p.Account(ctx)
		res.Session, res.Browser = sessionOf(ctx, p)
	}
	res.State, res.Error = checkState(err)
	if err == nil && id == factorio.Platform && res.Account != "" {
		// The portal user a one-click sign-in found is kept apart from the
		// session: «Выйти» drops the session, public reads keep the user.
		m.setAuthor(id, res.Account)
	}
	if err == nil {
		m.group.Trigger() // read what the account now reaches
	}
	return m.finish(ctx, id, res), nil
}

func (m *modPlatforms) finish(ctx context.Context, id string, res api.PlatformStatus) api.PlatformStatus {
	m.mu.Lock()
	m.checks[id] = res
	m.mu.Unlock()
	states := m.watch(all(m.Platforms(ctx)))
	if ps, ok := states[id]; ok {
		return ps
	}
	return res
}

func all(list []api.PlatformStatus) map[string]api.PlatformStatus {
	out := make(map[string]api.PlatformStatus, len(list))
	for _, ps := range list {
		out[ps.ID] = ps
	}
	return out
}

// watchRelogin shows «войдите снова» once per platform whose session expired.
func (m *modPlatforms) watchRelogin(ctx context.Context) { m.watch(all(m.Platforms(ctx))) }

func (m *modPlatforms) watch(states map[string]api.PlatformStatus) map[string]api.PlatformStatus {
	var show []api.PlatformStatus
	m.mu.Lock()
	for id, ps := range states {
		switch ps.State {
		case api.PlatformRelogin:
			if !m.notified[id] {
				m.notified[id] = true
				show = append(show, ps)
			}
		case api.PlatformConnected:
			m.notified[id] = false
		}
	}
	m.mu.Unlock()
	for _, ps := range show {
		m.log.Warn("platform session expired", "platform", ps.ID, "err", ps.Error)
		if m.onRelogin != nil {
			m.onRelogin(ps.ID, ps.Name)
		}
	}
	return states
}

// loginTimeout bounds one sign-in call (the MCP server may start a browser).
const loginTimeout = 90 * time.Second

// Login implements api.Platforms: «Подключить». Steam starts a QR sign-in;
// Nexus / CurseForge are switched on if needed, then their server imports a
// browser session or opens its sign-in window.
func (m *modPlatforms) Login(ctx context.Context, id string) (api.LoginStatus, error) {
	m.mu.Lock()
	delete(m.lastLogin, id) // a new attempt logs its states afresh
	m.mu.Unlock()
	m.log.Info("platform sign-in started", "platform", id)
	switch id {
	case steam.Platform:
		st, err := m.steam.StartQR(ctx)
		if err != nil {
			return api.LoginStatus{}, err
		}
		out := qrStatus(st)
		m.logLogin(out, sourceName(api.SessionQR), "")
		return out, nil
	case nexus.Platform, curseforge.Platform, factorio.Platform:
	default:
		return api.LoginStatus{}, api.ErrUnknownPlatform
	}
	if err := m.enable(id); err != nil {
		return api.LoginStatus{}, err
	}
	l, err := m.loginer(id)
	if err != nil {
		return api.LoginStatus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	// Already signed in (or a window already open): no new import or window.
	// The native sign-in checks its stored session itself (with a probe).
	if res, err := l.LoginStatus(ctx); err == nil && (res.InProgress || (res.LoggedIn && !m.isNative(id))) {
		return m.loginResult(ctx, id, res), nil
	}
	res, err := l.Login(ctx)
	if err != nil {
		return api.LoginStatus{}, err
	}
	return m.loginResult(ctx, id, res), nil
}

// LoginStatus implements api.Platforms: the progress the dashboard polls.
func (m *modPlatforms) LoginStatus(ctx context.Context, id string) (api.LoginStatus, error) {
	switch id {
	case steam.Platform:
		out := qrStatus(m.steam.QRLoginStatus())
		m.logLogin(out, sourceName(api.SessionQR), "")
		return out, nil
	case nexus.Platform, curseforge.Platform, factorio.Platform:
	default:
		return api.LoginStatus{}, api.ErrUnknownPlatform
	}
	l, err := m.loginer(id)
	if err != nil {
		return api.LoginStatus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	res, err := l.LoginStatus(ctx)
	if err != nil {
		return api.LoginStatus{}, err
	}
	return m.loginResult(ctx, id, res), nil
}

// CancelLogin implements api.Platforms (Steam QR; a server's window closes itself).
func (m *modPlatforms) CancelLogin(id string) error {
	switch id {
	case steam.Platform:
		m.steam.CancelQR()
		return nil
	case nexus.Platform, curseforge.Platform, factorio.Platform:
		// Stop a waiting sign-in (default-browser polling / the server's window).
		l, err := m.loginer(id)
		if err != nil {
			return nil // switched off: nothing is waiting
		}
		c, ok := l.(provider.LoginCanceller)
		if !ok {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.CancelLogin(ctx); err != nil {
			m.log.Warn("platform sign-in cancel failed", "platform", id, "err", err) // older server: its window times out
		} else {
			m.log.Info("platform sign-in cancelled", "platform", id)
		}
		return nil
	}
	return api.ErrUnknownPlatform
}

func qrStatus(s steam.QRStatus) api.LoginStatus {
	out := api.LoginStatus{Platform: steam.Platform, ChallengeURL: s.ChallengeURL, Error: s.Error}
	switch s.State {
	case steam.QRPending:
		out.State = api.LoginQR
	case steam.QRScanned:
		out.State = api.LoginScanned
	case steam.QRDone:
		out.State, out.Account = api.LoginConnected, s.SteamID
	case steam.QRExpired:
		out.State = api.LoginExpired
	case steam.QRFailed:
		out.State = api.LoginFailed
	default:
		out.State = api.LoginIdle
	}
	return out
}

func (m *modPlatforms) loginResult(ctx context.Context, id string, l provider.Login) api.LoginStatus {
	out := api.LoginStatus{Platform: id, State: api.LoginIdle, Account: l.Account}
	switch {
	case l.LoggedIn:
		out.State = api.LoginConnected
		m.connected(ctx, id)
	case l.InProgress || l.Window:
		out.State, out.Via, out.Browser = api.LoginWindow, l.Via, l.ViaBrowser
	default:
		out.Error = l.Detail
	}
	source, browser := sourceName(l.Source), l.Browser
	if out.State == api.LoginWindow {
		source, browser = cmpStr(l.Via, "window"), l.ViaBrowser
	}
	m.logLogin(out, source, browser)
	return out
}

// logLogin writes each sign-in state change once (the dashboard polls every
// few seconds): the platform, how the session arrived and why it failed.
func (m *modPlatforms) logLogin(st api.LoginStatus, source, browser string) {
	m.mu.Lock()
	if m.lastLogin == nil {
		m.lastLogin = map[string]string{}
	}
	changed := m.lastLogin[st.Platform] != st.State
	m.lastLogin[st.Platform] = st.State
	m.mu.Unlock()
	if !changed {
		return
	}
	attrs := []any{"platform", st.Platform, "state", st.State}
	if source != "" {
		attrs = append(attrs, "source", source)
	}
	if browser != "" {
		attrs = append(attrs, "browser", browser)
	}
	if st.Account != "" {
		attrs = append(attrs, "account", st.Account)
	}
	switch st.State {
	case api.LoginConnected:
		m.log.Info("platform sign-in succeeded", attrs...)
	case api.LoginFailed, api.LoginExpired:
		m.log.Warn("platform sign-in failed", append(attrs, "reason", cmpStr(st.Error, st.State))...)
	case api.LoginIdle:
		if st.Error != "" {
			m.log.Warn("platform sign-in ended without a session", append(attrs, "reason", st.Error)...)
		}
	default:
		m.log.Info("platform sign-in waiting", attrs...)
	}
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sourceName is the log word for a session source.
func sourceName(s string) string {
	switch s {
	case api.SessionBrowser:
		return "browser-session"
	case api.SessionWindow:
		return "login window"
	case api.SessionQR:
		return "QR"
	}
	return s
}

// sessionOf asks a Loginer where its session came from (none = public reads).
func sessionOf(ctx context.Context, p provider.Provider) (string, string) {
	l, ok := p.(provider.Loginer)
	if !ok {
		return "", ""
	}
	st, err := l.LoginStatus(ctx)
	switch {
	case err != nil:
		return "", ""
	case !st.LoggedIn:
		return api.SessionNone, ""
	case st.Source == api.SessionBrowser || st.Source == api.SessionWindow || st.Source == api.SessionManual || st.Source == api.SessionProfile:
		return st.Source, st.Browser
	default:
		return api.SessionStored, ""
	}
}

// refreshSessions learns, after a sync, where each running MCP platform's
// session came from (a restart forgets it; the card shows it without a «Проверить»).
func (m *modPlatforms) refreshSessions(ctx context.Context) {
	m.mu.Lock()
	var todo []provider.Provider
	var ids []string
	for id, mp := range m.live {
		if m.sessions[id][0] == "" {
			todo, ids = append(todo, mp.prov), append(ids, id)
		}
	}
	m.mu.Unlock()
	for i, p := range todo {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s, b := sessionOf(cctx, p)
		cancel()
		if s == "" {
			continue
		}
		m.mu.Lock()
		if m.sessions == nil {
			m.sessions = map[string][2]string{}
		}
		if m.live[ids[i]] != nil {
			m.sessions[ids[i]] = [2]string{s, b}
		}
		m.mu.Unlock()
	}
}

// steamSession maps the Steam settings view to PlatformStatus.Session.
func steamSession(st steam.Status) string {
	switch {
	case !st.HasCookies:
		return api.SessionNone
	case st.SignedIn:
		return api.SessionQR
	default:
		return api.SessionManual
	}
}

// Logout implements api.Platforms: «Выйти» drops the web session (public
// reads go on); forget («Отключить») also clears the detected identity —
// Steam's SteamID and key, Nexus / CurseForge's uploader account — and
// switches Nexus / CurseForge off, so the card reads «Не подключено».
func (m *modPlatforms) Logout(ctx context.Context, id string, forget bool) (api.PlatformStatus, error) {
	switch id {
	case steam.Platform:
		if err := m.steam.Logout(ctx); err != nil {
			return api.PlatformStatus{}, err
		}
		if forget {
			empty := ""
			if _, err := m.steam.Save(steam.Update{SteamID: &empty, APIKey: &empty}); err != nil {
				return api.PlatformStatus{}, err
			}
		}
	case nexus.Platform, curseforge.Platform, factorio.Platform:
		ctx, cancel := context.WithTimeout(ctx, loginTimeout)
		defer cancel()
		if err := m.serverLogout(ctx, id); err != nil {
			return api.PlatformStatus{}, err
		}
		if forget {
			if err := m.forget(id); err != nil {
				return api.PlatformStatus{}, err
			}
		}
	default:
		return api.PlatformStatus{}, api.ErrUnknownPlatform
	}
	m.log.Info("platform signed out", "platform", id, "forget", forget)
	m.mu.Lock()
	delete(m.sessions, id)
	delete(m.checks, id)
	delete(m.lastLogin, id)
	m.mu.Unlock()
	if !forget {
		return m.Check(ctx, id) // read-only state, freshly checked
	}
	states := all(m.Platforms(ctx))
	return states[id], nil
}

// serverLogout runs the MCP server's logout tool; a switched-off platform
// gets a short-lived server for it (the session file is the server's own).
func (m *modPlatforms) serverLogout(ctx context.Context, id string) error {
	m.mu.Lock()
	mp := m.live[id]
	m.mu.Unlock()
	var p provider.Provider
	switch {
	case mp != nil:
		p = mp.prov
	case nativeEngine(id, modConfig(m.cfgs.Get().Providers, id)):
		p, _ = m.buildNative(id, func() string { return "" }) // clears the stored jar + profile of a switched-off platform
	default:
		opts := mcpbridge.Options{Name: id, Log: m.log, Command: func() (string, []string) {
			s := modConfig(m.cfgs.Get().Providers, id).MCP
			return os.ExpandEnv(s.Command), s.Args
		}}
		if m.dial != nil {
			opts.Dial = m.dial(id)
		}
		b := mcpbridge.New(opts)
		defer b.Close()
		if id == nexus.Platform {
			p = nexus.New(nexus.Options{Bridge: b, Log: m.log, Author: func() string { return "" }})
		} else {
			p = curseforge.New(curseforge.Options{Bridge: b, Log: m.log, Author: func() string { return "" }})
		}
	}
	lo, ok := p.(provider.Logouter)
	if !ok {
		return api.ErrUnknownPlatform
	}
	return lo.Logout(ctx)
}

// isNative reports whether a running platform uses the built-in engine.
func (m *modPlatforms) isNative(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live[id].native()
}

// forget clears the uploader account and switches the platform off.
func (m *modPlatforms) forget(id string) error {
	for range 3 { // a concurrent settings write bumps the revision: retry
		cur := m.cfgs.Get()
		next, err := m.cfgs.Patch(cur.Revision, []byte(`{"providers":{"`+id+`":{"enabled":false,"author":""}}}`), nil)
		if err == nil {
			m.apply(next)
			return nil
		}
		if !errors.Is(err, config.ErrConflict) {
			return err
		}
	}
	return errors.New("settings keep changing; try again")
}

func (m *modPlatforms) loginer(id string) (provider.Loginer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mp := m.live[id]
	if mp == nil {
		return nil, errors.New(id + " is switched off")
	}
	l, ok := mp.prov.(provider.Loginer)
	if !ok {
		return nil, api.ErrUnknownPlatform
	}
	return l, nil
}

// enable switches a mod platform on (providers.<id>.enabled); apply starts it.
func (m *modPlatforms) enable(id string) error {
	for range 3 { // a concurrent settings write bumps the revision: retry
		cur := m.cfgs.Get()
		if modConfig(cur.Providers, id).Enabled {
			return nil
		}
		next, err := m.cfgs.Patch(cur.Revision, []byte(`{"providers":{"`+id+`":{"enabled":true}}}`), nil)
		if err == nil {
			m.apply(next) // main's settings subscriber does it too; apply is idempotent
			return nil
		}
		if !errors.Is(err, config.ErrConflict) {
			return err
		}
	}
	return errors.New("settings keep changing; try again")
}

// connected runs after a sign-in: Nexus remembers the member as the account
// (reads keep working if the session later expires), the platform is checked
// (which also starts a sync) and a new expiry may notify again.
func (m *modPlatforms) connected(ctx context.Context, id string) {
	if id == nexus.Platform {
		m.mu.Lock()
		mp := m.live[id]
		m.mu.Unlock()
		if np, ok := mp.provOK(); ok && strings.TrimSpace(mp.cfg.Author) == "" {
			if np.Member() > 0 {
				m.setAuthor(id, strconv.Itoa(np.Member()))
			} else if mp.native() && mp.session != nil && mp.session.Status().Account != "" {
				m.setAuthor(id, mp.session.Status().Account) // the native sign-in knows the account name
			}
		}
	}
	m.mu.Lock()
	m.notified[id] = false
	delete(m.checks, id)
	delete(m.sessions, id)
	m.mu.Unlock()
	go func() {
		if _, err := m.Check(context.WithoutCancel(ctx), id); err != nil {
			m.log.Error("platform check after sign-in", "platform", id, "err", err)
		}
	}()
}

func (mp *modPlatform) provOK() (*nexus.Provider, bool) {
	if mp == nil {
		return nil, false
	}
	np, ok := mp.prov.(*nexus.Provider)
	return np, ok
}

func (m *modPlatforms) setAuthor(id, author string) {
	for range 3 {
		cur := m.cfgs.Get()
		if strings.TrimSpace(modConfig(cur.Providers, id).Author) != "" {
			return
		}
		patch, _ := json.Marshal(map[string]any{"providers": map[string]any{id: map[string]any{"author": author}}})
		_, err := m.cfgs.Patch(cur.Revision, patch, nil)
		if err == nil {
			m.log.Info("platform account detected", "platform", id, "author", author)
			return
		}
		if !errors.Is(err, config.ErrConflict) {
			m.log.Error("store the detected account", "platform", id, "err", err)
			return
		}
	}
}

// checkState maps an Account error to a platform state and its reason.
func checkState(err error) (string, string) {
	switch {
	case err == nil:
		return api.PlatformConnected, ""
	case errors.Is(err, mcpbridge.ErrUnavailable):
		return api.PlatformUnavailable, err.Error()
	case errors.Is(err, curseforge.ErrRelogin), errors.Is(err, steam.ErrSessionExpired), errors.Is(err, provider.ErrRelogin):
		return api.PlatformRelogin, err.Error()
	case errors.Is(err, provider.ErrNotSignedIn):
		return api.PlatformSignedOut, err.Error()
	default:
		return api.PlatformError, err.Error()
	}
}
