// Command issuewatcher is the desktop hub: a tray icon plus a loopback web UI
// opened in the user's default browser, all state in data\ next to the exe.
package main

// The exe's manifest (per-monitor v2 DPI awareness, common controls v6) lives in
// rsrc_windows_amd64.syso, generated from winres/winres.json:
//
//go:generate go run github.com/tc-hib/go-winres@v0.3.3 make --in ../../winres/winres.json --arch amd64 --out rsrc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	issuewatcher "github.com/UberMorgott/issuewatcher"
	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/autostart"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/instance"
	"github.com/UberMorgott/issuewatcher/internal/notify"
	"github.com/UberMorgott/issuewatcher/internal/paths"
	folderpicker "github.com/UberMorgott/issuewatcher/internal/picker"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/provider/steam"
	"github.com/UberMorgott/issuewatcher/internal/provider/steamugc"
	"github.com/UberMorgott/issuewatcher/internal/redact"
	"github.com/UberMorgott/issuewatcher/internal/release"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// Version is set at build time via -ldflags "-X main.Version=...".
var Version = "dev"

// Dev/verification switches (environment variables).
const (
	envPort      = "IW_PORT"           // preferred port, used only if free
	envNoBrowser = "IW_NO_BROWSER"     // =1: log browser URLs instead of opening them
	envDemo      = "IW_DEMO"           // =1: scripted badge + popup cards + simulated click
	envDebug     = "IW_DEBUG"          // =1: debug log level (tray callbacks except mouse moves)
	envSnapshot  = "IW_POPUP_SNAPSHOT" // =<dir>: render sample popup PNG files there and exit
	// =1: server + sync only, for automated runs: no tray icon, no popups, no
	// browser launch (URLs are logged), no window focusing, no autostart registry writes.
	envHeadless = "IW_HEADLESS"
	// Dev/E2E only: GitHub REST + GraphQL base (e.g. a githubtest fake from
	// tools/fakegithub) and the web base for OAuth/device endpoints (default:
	// the API base). The user token goes there: never set it with real credentials.
	envGitHubAPI = "IW_GITHUB_API"
	envGitHubWeb = "IW_GITHUB_WEB"
	// Tests only, with the browser suppressed: =<file> gets the latest launch
	// URL (with its one-time token, which the log masks).
	envLaunchFile = "IW_LAUNCH_FILE"
)

// newAuth is the GitHub sign-in owner for dataDir, with the dev endpoint override.
func newAuth(log *slog.Logger, dataDir string) *github.Auth {
	auth := github.NewAuth(filepath.Join(dataDir, "secrets"))
	if api := strings.TrimRight(os.Getenv(envGitHubAPI), "/"); api != "" {
		auth.APIURL, auth.WebURL = api, api
		if web := strings.TrimRight(os.Getenv(envGitHubWeb), "/"); web != "" {
			auth.WebURL = web
		}
		log.Warn("dev: GitHub endpoints overridden", "api", auth.APIURL, "web", auth.WebURL)
	}
	return auth
}

// snapshotScale renders IW_POPUP_SNAPSHOT samples at 144 DPI.
const snapshotScale = 1.5

func main() {
	if len(os.Args) > 1 && os.Args[1] == steamugc.HelperArg {
		// Steam Workshop helper (steamugc): ISteamUGC through the running Steam client, one job on stdin.
		os.Exit(steamugc.Main())
	}
	if isCLI(os.Args[1:]) {
		// CLI/MCP subcommands talk to the running app; they run before the
		// single-instance lock and never open the tray, DB or a message box.
		os.Exit(cliMain(os.Args[1:]))
	}
	if exe, err := os.Executable(); err == nil && isCLICopy(exe) {
		// The console copy is for shells only: the desktop app is issuewatcher.exe.
		_, _ = os.Stderr.WriteString(cliUsage)
		os.Exit(exitUsage)
	}
	if err := run(); err != nil {
		// Release builds have no console: show the reason (never block an automated run).
		if os.Getenv(envHeadless) != "1" {
			msgBox("IssueWatcher не запустился", redact.String(err.Error()))
		}
		os.Exit(1)
	}
}

func run() (err error) {
	if dir := os.Getenv(envSnapshot); dir != "" {
		_, err := notify.WriteSnapshots(dir, snapshotScale)
		return err
	}
	dataDir, err := paths.DataDir()
	if err != nil {
		return err
	}
	log, closeLog, err := openLog(dataDir)
	if err != nil {
		return err
	}
	defer closeLog()

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	l := parseLaunch(os.Args[1:])
	var serving atomic.Bool // the port is bound: past the point where a new version rolls back
	if l.afterUpdate > 0 {
		defer func() {
			if err != nil && !serving.Load() {
				err = rollbackUpdate(log, dataDir, exe, l, err)
			}
		}()
	}
	if err := l.handOver(log, dataDir); err != nil {
		return err
	}

	lock, err := acquireLock(dataDir, l)
	if errors.Is(err, instance.ErrAlreadyRunning) {
		if startMinimized(os.Args[1:], false) {
			log.Info("second launch with --minimized: already running, nothing to show")
			return nil
		}
		return handOff(log, dataDir)
	}
	if err != nil {
		return err
	}
	defer lock.Release()
	log.Info("starting", "version", Version, "data_dir", dataDir, "pid", os.Getpid())

	dbPath := filepath.Join(dataDir, "issuewatcher.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	rdb, err := store.OpenReader(dbPath, 4)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	cfgs, err := config.Open(dataDir)
	if err != nil {
		return err
	}
	cfg := cfgs.Get()
	headless := os.Getenv(envHeadless) == "1"
	var entry runEntry = autostart.Default
	if headless {
		entry = &memEntry{} // never touch the real Run key from an automated run
		log.Info("headless: no tray, no popups, no browser, no autostart writes")
	} else if wrote, err := autostart.Default.Refresh(cfg.General.StartWithWindows, exe); err != nil {
		// Portable folder moved: keep the autostart entry pointing at this exe.
		log.Error("autostart refresh", "err", err)
	} else if wrote {
		log.Info("autostart entry updated", "exe", exe)
	}
	settings := &appSettings{store: cfgs, dataDir: dataDir, exe: exe, version: Version, entry: entry}
	minimized := startMinimized(os.Args[1:], cfg.General.StartMinimized) || l.quiet()
	return serve(log, dataDir, cfgs, store.NewWithReader(db, rdb), newAuth(log, dataDir), settings, minimized, headless, l, &serving)
}

func serve(log *slog.Logger, dataDir string, cfgs *config.Store, st *store.Store, auth *github.Auth,
	settings *appSettings, minimized, headless bool, l launch, serving *atomic.Bool,
) error {
	open := func(u string) { go openBrowser(log, u) }
	if headless || os.Getenv(envNoBrowser) == "1" {
		launchFile := os.Getenv(envLaunchFile)
		open = func(u string) {
			log.Info("browser open suppressed", "url", u) // the handler masks the one-time token
			if launchFile != "" {
				if err := os.WriteFile(launchFile, []byte(u), 0o600); err != nil {
					log.Error("write launch file", "err", err)
				}
			}
		}
	}
	preferred, _ := strconv.Atoi(os.Getenv(envPort))
	if app, err := auth.App(); preferred == 0 && err == nil {
		// Reuse the port baked into the app's callback URL when it is free, so
		// sign-in works even if GitHub matched loopback redirect ports exactly.
		preferred = app.RegisteredPort
	}
	if preferred == 0 {
		preferred = instance.ReadPort(dataDir) // open tabs reconnect after a restart
	}
	requirePort := l.afterUpdate > 0
	if requirePort {
		// The tabs of the old version reconnect only to the same origin: take
		// its port or fail (and roll back), never move to another one.
		preferred = instance.ReadPort(dataDir)
	}

	var (
		tray     *notify.Tray // set once in the RunTray ready callback, before any handler runs
		setBadge func(int64)
		srv      *api.Server // set below, before the syncer runs
	)
	refreshBadge := func() {
		n, err := st.UnreadCount(context.Background())
		if err != nil {
			log.Error("unread count", "err", err)
			return
		}
		setBadge(int64(n))
	}
	cfg := cfgs.Get()
	prefs := notifyPrefs(cfg.Notifications)
	var live atomic.Pointer[notify.Prefs] // in-app toast filter (kinds, muted projects)
	lp := liveFilter(cfg.Notifications)
	live.Store(&lp)
	var group atomic.Bool
	group.Store(cfg.Notifications.Group)
	gh := github.NewProvider(auth)
	var jobs *runner.Runner      // set below; the sync loop starts after it exists
	var releases *release.Engine // set below; autopilot: inbox → fix runs → releases
	onUpdate := func(events []store.Event, unread int) {
		popups := events
		if group.Load() {
			popups = groupRepeats(events)
		}
		if tray != nil {
			tray.NotifyEvents(popups) // own popups, filtered by notify.Prefs (Tray.SetPrefs)
		}
		setBadge(int64(unread))
		publishLive(srv, live.Load().Filter(events, time.Now()))
		// Mod pages named like exactly one GitHub repo link themselves (once).
		if n, err := st.AutoLink(context.Background()); err != nil {
			log.Error("auto-link mod pages", "err", err)
		} else if n > 0 {
			log.Info("mod pages linked to their code projects", "links", n)
			if srv != nil {
				srv.Publish(api.EventDataChanged, api.DataChange{Reason: "links"})
			}
		}
		if jobs != nil {
			jobs.Automate(context.Background(), events) // rules → rule jobs (off by default)
			jobs.Refresh()                              // a closed issue ends its direct fix job
		}
		if releases != nil {
			releases.Kick() // new inbox events → fix runs
		}
	}
	// One syncer per connected account (source); GitHub is the primary one.
	stm := steam.New(steam.Options{Dir: filepath.Join(dataDir, "secrets"), Log: log}) // keyless reads once a SteamID is set
	sy := syncer.NewGroup(syncer.New(syncer.Options{
		Store: st, Provider: gh, Plan: syncPlan(cfg.Sync), Log: log, OnUpdate: onUpdate,
	}), syncer.New(syncer.Options{
		Store: st, Provider: stm, Plan: syncPlan(cfg.Sync), Log: log, OnUpdate: onUpdate,
	}))
	mods := newModPlatforms(cfgs, st, log, sy, gh, stm, onUpdate, dataDir) // Nexus / CurseForge / Factorio (native), switched live
	defer mods.Close()
	mods.onRelogin = func(id, name string) { // a click opens «Подключить» for that platform
		c := notify.ReloginCard(id, name, time.Now())
		if tray != nil {
			tray.ShowCards(c)
		}
	}
	// Settings apply live: popups, toasts, poll interval.
	cfgs.Subscribe(func(_, cur config.Settings) {
		lp := liveFilter(cur.Notifications)
		live.Store(&lp)
		group.Store(cur.Notifications.Group)
		sy.SetPlan(syncPlan(cur.Sync))
		mods.apply(cur)
		if tray != nil {
			tray.SetPrefs(notifyPrefs(cur.Notifications))
			tray.SetTheme(popupTheme(cur.Appearance))
		}
	})
	var testN atomic.Int64
	jobs = newRunner(log, dataDir, cfgs, st, gh, sy, func() *api.Server { return srv }, func() *notify.Tray { return tray })
	releases = newReleaseEngine(log, dataDir, cfgs, settings, st, gh, jobs, sy, mods.Publisher, func() *api.Server { return srv })

	focus := notify.FocusDashboard
	var picker api.FolderPicker
	if headless {
		focus = nil // never raise the user's own dashboard window; no desktop dialogs
	} else {
		fp := folderpicker.New(log)
		go fp.Warm() // COM + dialog class loaded before the first «Обзор…»
		picker = fp
	}
	// quit shuts the app down gracefully (tray loop or headless wait returns,
	// then HTTP/SSE, sync and SQLite close); the updater calls it once the new
	// version is ready. A hung shutdown ends hard so the new version can start.
	quit := make(chan struct{})
	var quitOnce sync.Once
	requestQuit := func() {
		quitOnce.Do(func() {
			log.Info("shutting down for the update")
			close(quit)
			if tray != nil {
				tray.Quit()
			}
			time.AfterFunc(shutdownLimit, func() {
				log.Error("shutdown did not finish in time; exiting", "limit", shutdownLimit)
				os.Exit(0)
			})
		})
	}
	last := selfupdate.TakeResult(dataDir)
	if l.afterUpdate > 0 {
		last = &selfupdate.Result{OK: true, From: l.from, To: Version, At: time.Now().UTC()}
	}
	upd := newUpdater(log, cfgs, dataDir, settings.exe, last, func(s selfupdate.Status) {
		if srv != nil {
			srv.Publish(api.EventUpdateStatus, s)
		}
	}, requestQuit)

	session, err := loadSession(filepath.Join(dataDir, "secrets"))
	if err != nil {
		log.Error("session secret: per-run fallback", "err", err) // tabs sign in again after a restart
	}
	srv, err = api.New(context.Background(), api.Options{
		Assets:         issuewatcher.Assets(),
		PreferredPort:  preferred,
		RequirePort:    requirePort,
		Updates:        upd,
		Version:        Version,
		Open:           open,
		Log:            log,
		GitHub:         auth,
		Store:          st,
		Sync:           sy,
		OnUnreadChange: refreshBadge,
		Settings:       settings,
		TestNotification: func() {
			c := notify.SampleCard(testN.Add(1), time.Now())
			if tray != nil {
				tray.ShowCards(c)
			}
			log.Info("test notification shown", "item", c.ItemID, "headless", tray == nil)
		},
		// Tray/notification clicks give this process foreground rights: bring the
		// dashboard's browser window to the front, or open a new tab.
		Focus:            focus,
		SessionSecret:    session,
		Runner:           jobs,
		Picker:           picker,
		Steam:            stm,
		NexusKey:         mods.nexusKeys,
		CurseForgeUpload: mods.cfUpload,
		SteamUpload:      mods.workshop,
		Workshop:         steamugc.New(steamugc.Options{DataDir: dataDir}),
		Publishers:       mods.Publisher,
		PageEditors:      mods.PageEditor,
		Platforms:        mods,
		Release:          releases,
	})
	if err != nil {
		return err
	}
	serving.Store(true)
	if err := instance.WritePort(dataDir, srv.Port()); err != nil {
		log.Error("remember port", "err", err)
	}
	if l.afterUpdate > 0 || l.rolledBack > 0 {
		log.Info("update hand-over done", "version", Version, "port", srv.Port(), "from", l.from, "rolled_back", l.rolledBack > 0)
		go cleanupAfterUpdate(log, dataDir, settings.exe)
	} else {
		_ = selfupdate.Cleanup(context.Background(), settings.exe, 1, 0) // leftovers of a crashed update
	}
	go refreshCLI(log, settings.exe) // past the rollback point: the copy matches the version that stays
	rt := instance.Runtime{
		PID: os.Getpid(), Port: srv.Port(), URL: srv.BaseURL(), Token: srv.Token(),
		Version: Version, StartedAt: time.Now().UTC(),
	}
	if err := instance.WriteRuntime(dataDir, rt); err != nil {
		return err
	}
	defer func() {
		if err := instance.RemoveRuntime(dataDir, rt.PID); err != nil {
			log.Error("remove runtime.json", "err", err)
		}
	}()
	log.Info("http listening", "url", srv.BaseURL())

	unread := atomic.Int64{}
	if n, err := st.UnreadCount(context.Background()); err == nil {
		unread.Store(int64(n))
	}
	setBadge = func(n int64) {
		unread.Store(n)
		if tray == nil {
			return // headless
		}
		icon, err := notify.TrayIcon(int(n))
		if err == nil {
			err = tray.SetIcon(icon)
		}
		log.Info("tray badge set", "count", n, "label", notify.BadgeLabel(int(n)), "err", err)
	}

	syncCtx, stopSync := context.WithCancel(context.Background())
	syncDone := make(chan struct{})
	startSync := func() {
		if err := jobs.Start(syncCtx); err != nil {
			log.Error("agent jobs: start", "err", err)
		}
		// Release runs left by a previous process: sending steps are probed, then
		// the running ones continue (docs/AUTOPILOT.md → crash resume).
		if err := releases.Start(syncCtx); err != nil {
			log.Error("release runs: start", "err", err)
		}
		go func() { sy.Run(syncCtx); close(syncDone) }()
		go upd.Run(syncCtx) // automatic update checks (never installs)
	}
	defer func() {
		// Graceful stop: the sync step in flight finishes before SQLite closes.
		stopSync()
		select {
		case <-syncDone:
		case <-time.After(10 * time.Second):
			log.Warn("sync did not stop in time")
		}
		// Running agents are killed by the cancelled context; wait for their outcome to be stored.
		stopped := make(chan struct{})
		go func() { jobs.Wait(); releases.Wait(); close(stopped) }() // a release step left sending is probed at the next start
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			log.Warn("agent jobs did not stop in time")
		}
	}()
	// What to show on start: after an update nothing (the open tab reconnects
	// and reloads); after a rollback the Updates page with the error.
	startPath := "/"
	switch {
	case minimized:
		startPath = ""
	case l.rolledBack > 0:
		startPath = "/settings/updates"
	}

	if headless {
		return runHeadless(syncCtx, log, srv, startSync, quit, startPath)
	}
	icon, err := notify.TrayIcon(int(unread.Load()))
	if err != nil {
		return err
	}
	err = notify.RunTray(notify.TrayOptions{
		Tooltip: "IssueWatcher",
		Icon:    icon,
		Log:     log,
		Theme:   popupTheme(cfg.Appearance), // settings.changed → Tray.SetTheme / SetPrefs
		Prefs:   &prefs,
		Menu: []notify.MenuItem{
			{Title: "Открыть", OnClick: func() { srv.OpenBrowser("") }}, // "": an open tab keeps its page
			{Title: "Проверить обновления", OnClick: func() {
				srv.OpenBrowser("/settings/updates")
				go func() { _, _ = upd.Check(context.Background()) }()
			}},
			{Title: "Тестовое уведомление", OnClick: func() {
				n := unread.Add(1)
				setBadge(n)
				c := notify.SampleCard(n, time.Now())
				tray.ShowCards(c)
				log.Info("test notification shown", "item", c.ItemID)
			}},
			{},
			{Title: "Выход", OnClick: func() { tray.Quit() }},
		},
		OnClick: func() {
			log.Info("tray click")
			srv.OpenBrowser("")
		},
		OnCardClick: func(path string) { srv.OpenBrowser(path) },
	}, func(t *notify.Tray) {
		tray = t
		go func() {
			if err := srv.Serve(); err != nil {
				log.Error("http server stopped", "err", err)
				t.Quit()
			}
		}()
		log.Info("tray ready", "badge", unread.Load())
		startSync()
		if startPath == "" {
			log.Info("started minimized: dashboard not opened", "after_update", l.afterUpdate > 0)
		} else {
			go srv.OpenOnStart(startPath, api.StartGrace) // a tab of the previous run may reconnect
		}
		if os.Getenv(envDemo) == "1" {
			go runDemo(log, t, setBadge)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if serr := srv.Shutdown(ctx); serr != nil {
		log.Error("http shutdown", "err", serr)
	}
	log.Info("exit", "tray_err", err)
	return err
}

// runDemo exercises badge, notification and the notification-click path so the
// shell can be verified from the log without a mouse.
func runDemo(log *slog.Logger, t *notify.Tray, setBadge func(int64)) {
	log.Info("demo: start")
	time.Sleep(time.Second)
	setBadge(150)
	for n := range int64(4) { // four cards: two shown + "+2 ещё"
		t.ShowCards(notify.SampleCard(n, time.Now()))
	}
	log.Info("demo: notifications shown")
	time.Sleep(4 * time.Second)
	log.Info("demo: simulating notification click")
	t.SimulateCardClick()
	time.Sleep(time.Second)
	setBadge(5)
	log.Info("demo: done")
}

// handOff runs in a second launch: ask the running instance to open the browser.
func handOff(log *slog.Logger, dataDir string) error {
	var (
		rt  instance.Runtime
		err error
	)
	for range 20 { // the first instance may still be starting
		if rt, err = instance.ReadRuntime(dataDir); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("already running, but %w", err)
	}

	body, _ := json.Marshal(map[string]string{"path": "/"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.URL+"/api/open", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+rt.Token)
	// This launch came from the user: let the running instance take the foreground.
	allowForeground(rt.PID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("already running (pid %d) but not reachable: %w", rt.PID, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("already running (pid %d): open request failed: %s", rt.PID, resp.Status)
	}
	log.Info("second launch: handed off to running instance", "pid", rt.PID, "url", rt.URL, "self", os.Getpid())
	return nil
}

// openBrowser opens url in the default browser.
func openBrowser(log *slog.Logger, u string) {
	// Bounded: rundll32 returns once the URL is handed to the browser.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rundll32.exe", "url.dll,FileProtocolHandler", u) //nolint:gosec // G204: fixed binary; u is our own loopback URL
	if err := cmd.Run(); err != nil {
		log.Error("open browser", "err", err)
	}
}

// openLog writes to data\logs\issuewatcher.log and stderr; fatal panics go there too.
func openLog(dataDir string) (*slog.Logger, func(), error) {
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("log dir: %w", err)
	}
	logPath := filepath.Join(logDir, "issuewatcher.log")
	// Older versions logged the one-time sign-in URL in full: mask it once.
	redactErr := redact.File(logPath)
	f, err := os.OpenFile(logPath, //nolint:gosec // G304: fixed name inside our own data dir
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("crash output: %w", err)
	}
	level := slog.LevelInfo
	if os.Getenv(envDebug) == "1" {
		level = slog.LevelDebug
	}
	// Every line goes through redact: tokens, OAuth codes, keys and cookies never reach the file or stderr.
	log := slog.New(redact.NewHandler(slog.NewTextHandler(io.MultiWriter(f, os.Stderr), &slog.HandlerOptions{Level: level})))
	if redactErr != nil {
		log.Warn("log: masking secrets in the old log failed", "err", redactErr)
	}
	return log, func() { _ = f.Close() }, nil
}

var procAllowSetForegroundWindow = windows.NewLazySystemDLL("user32.dll").NewProc("AllowSetForegroundWindow")

// allowForeground lets process pid bring a window to the front (best effort).
func allowForeground(pid int) {
	_, _, _ = procAllowSetForegroundWindow.Call(uintptr(pid))
}

func msgBox(caption, text string) {
	c, err1 := windows.UTF16PtrFromString(caption)
	t, err2 := windows.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	_, _ = windows.MessageBox(0, t, c, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND)
}
