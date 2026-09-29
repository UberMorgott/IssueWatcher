package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/curseforge"
	"github.com/UberMorgott/issuewatcher/internal/provider/mcpbridge"
	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// checkTimeout bounds one «Проверить» (an MCP server may start a browser first).
const checkTimeout = 90 * time.Second

// modPlatform is one running MCP-backed platform.
type modPlatform struct {
	bridge *mcpbridge.Client
	prov   provider.Provider
	syncer *syncer.Syncer
	cfg    config.ModPlatform
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

	// onRelogin shows the tray card «<platform>: войдите снова» (set by main).
	onRelogin func(id, name string)
	// dial replaces the server command (tests: in-memory MCP servers).
	dial func(id string) func(ctx context.Context) (mcp.Transport, error)

	mu       sync.Mutex
	live     map[string]*modPlatform
	checks   map[string]api.PlatformStatus // last «Проверить» result per platform
	notified map[string]bool               // relogin card shown since the platform was last connected
}

func newModPlatforms(cfgs *config.Store, st *store.Store, log *slog.Logger, group *syncer.Group,
	gh provider.Provider, stm *steam.Provider, onUpdate func([]store.Event, int),
) *modPlatforms {
	m := &modPlatforms{cfgs: cfgs, st: st, log: log, group: group, gh: gh, steam: stm, onUpdate: onUpdate,
		live: map[string]*modPlatform{}, checks: map[string]api.PlatformStatus{}, notified: map[string]bool{}}
	m.apply(cfgs.Get())
	group.OnProgress(func(p syncer.Progress) {
		if p.State == syncer.ProgressDone || p.State == syncer.ProgressError {
			go m.watchRelogin(context.Background())
		}
	})
	stm.OnSignIn(func() { go m.connected(context.Background(), steam.Platform) })
	return m
}

// Close stops the MCP server children for good.
func (m *modPlatforms) Close() {
	m.mu.Lock()
	bridges := make([]*mcpbridge.Client, 0, len(m.live))
	for _, p := range m.live {
		bridges = append(bridges, p.bridge)
	}
	m.mu.Unlock()
	for _, b := range bridges {
		b.Close()
	}
}

func modConfig(p config.Providers, id string) config.ModPlatform {
	if id == nexus.Platform {
		return p.Nexus
	}
	return p.CurseForge
}

// apply brings the running platforms in line with the settings. Bridges are
// stopped after m.mu is released: a stop may wait for an in-flight call, and
// Platforms (GET /api/platforms) must not wait behind it.
func (m *modPlatforms) apply(cfg config.Settings) {
	var closing, stopping []*mcpbridge.Client
	defer func() {
		for _, b := range closing {
			b.Close()
		}
		for _, b := range stopping {
			b.Stop()
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range []string{nexus.Platform, curseforge.Platform} {
		want := modConfig(cfg.Providers, id)
		cur := m.live[id]
		switch {
		case want.Enabled && cur == nil:
			p := m.start(id, cfg)
			m.live[id] = p
			m.group.Add(p.syncer)
			m.log.Info("mod platform on", "platform", id)
		case !want.Enabled && cur != nil:
			m.group.Remove(id)
			closing = append(closing, cur.bridge) // final: a late call must not start an unowned server
			delete(m.live, id)
			delete(m.checks, id)
			m.log.Info("mod platform off", "platform", id)
		case cur != nil && (want.MCP.Command != cur.cfg.MCP.Command || !slices.Equal(want.MCP.Args, cur.cfg.MCP.Args)):
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

// start builds a platform's bridge, provider and syncer. m.mu must be held.
func (m *modPlatforms) start(id string, cfg config.Settings) *modPlatform {
	pick := func() config.ModPlatform { return modConfig(m.cfgs.Get().Providers, id) }
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
	"github": "GitHub", nexus.Platform: "Nexus Mods", curseforge.Platform: "CurseForge", steam.Platform: "Steam Workshop",
}

// Platforms implements api.Platforms: every platform, GitHub first.
func (m *modPlatforms) Platforms(ctx context.Context) []api.PlatformStatus {
	sources := map[string]syncer.SourceStatus{}
	projects := map[string]int{}
	for _, s := range m.group.Status().Sources {
		sources[s.Platform] = s
	}
	if repos, err := m.st.Repos(ctx); err == nil {
		for _, r := range repos {
			projects[r.Platform]++
		}
	}
	cfg := m.cfgs.Get().Providers
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.PlatformStatus, 0, 4)
	for _, id := range []string{"github", nexus.Platform, curseforge.Platform, steam.Platform} {
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
		default:
			ps.Enabled = modConfig(cfg, id).Enabled
			if p := m.live[id]; p != nil {
				ps.Capabilities = p.prov.Capabilities()
				ps.Running = p.bridge.Running()
			} else if id == nexus.Platform {
				ps.Capabilities = nexus.New(nexus.Options{}).Capabilities()
			} else {
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
	case nexus.Platform, curseforge.Platform:
		if mp := m.live[id]; mp != nil {
			p = mp.prov
		}
	}
	m.mu.Unlock()
	if id != steam.Platform && id != nexus.Platform && id != curseforge.Platform {
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
	} else {
		res.Account, err = p.Account(ctx)
	}
	res.State, res.Error = checkState(err)
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
	switch id {
	case steam.Platform:
		st, err := m.steam.StartQR(ctx)
		if err != nil {
			return api.LoginStatus{}, err
		}
		return qrStatus(st), nil
	case nexus.Platform, curseforge.Platform:
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
	if res, err := l.LoginStatus(ctx); err == nil && (res.LoggedIn || res.InProgress) {
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
		return qrStatus(m.steam.QRLoginStatus()), nil
	case nexus.Platform, curseforge.Platform:
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
	case nexus.Platform, curseforge.Platform:
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
		out.State = api.LoginWindow
	default:
		out.Error = l.Detail
	}
	return out
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
		if np, ok := mp.provOK(); ok && np.Member() > 0 && strings.TrimSpace(mp.cfg.Author) == "" {
			m.setAuthor(id, strconv.Itoa(np.Member()))
		}
	}
	m.mu.Lock()
	m.notified[id] = false
	delete(m.checks, id)
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
		_, err := m.cfgs.Patch(cur.Revision, []byte(`{"providers":{"`+id+`":{"author":"`+author+`"}}}`), nil)
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
	case errors.Is(err, curseforge.ErrRelogin), errors.Is(err, steam.ErrSessionExpired):
		return api.PlatformRelogin, err.Error()
	case errors.Is(err, provider.ErrNotSignedIn):
		return api.PlatformSignedOut, err.Error()
	default:
		return api.PlatformError, err.Error()
	}
}
