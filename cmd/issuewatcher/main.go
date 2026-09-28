// Command issuewatcher is the desktop hub: a tray icon plus a loopback web UI
// opened in the user's default browser, all state in data\ next to the exe.
package main

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
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
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
)

// snapshotScale renders IW_POPUP_SNAPSHOT samples at 144 DPI.
const snapshotScale = 1.5

func main() {
	if err := run(); err != nil {
		// Release builds have no console: show the reason.
		msgBox("IssueWatcher не запустился", err.Error())
		os.Exit(1)
	}
}

func run() error {
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

	lock, err := instance.Acquire(dataDir)
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

	db, err := store.Open(context.Background(), filepath.Join(dataDir, "issuewatcher.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	cfgs, err := config.Open(dataDir)
	if err != nil {
		return err
	}
	cfg := cfgs.Get()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	// Portable folder moved: keep the autostart entry pointing at this exe.
	if wrote, err := autostart.Default.Refresh(cfg.General.StartWithWindows, exe); err != nil {
		log.Error("autostart refresh", "err", err)
	} else if wrote {
		log.Info("autostart entry updated", "exe", exe)
	}
	settings := &appSettings{store: cfgs, dataDir: dataDir, exe: exe, version: Version, entry: autostart.Default}
	minimized := startMinimized(os.Args[1:], cfg.General.StartMinimized)
	return serve(log, dataDir, cfgs, store.New(db), github.NewAuth(filepath.Join(dataDir, "secrets")), settings, minimized)
}

func serve(log *slog.Logger, dataDir string, cfgs *config.Store, st *store.Store, auth *github.Auth,
	settings *appSettings, minimized bool,
) error {
	open := func(u string) { go openBrowser(log, u) }
	if os.Getenv(envNoBrowser) == "1" {
		open = func(u string) { log.Info("browser open suppressed", "url", u) }
	}
	preferred, _ := strconv.Atoi(os.Getenv(envPort))
	if app, err := auth.App(); preferred == 0 && err == nil {
		// Reuse the port baked into the app's callback URL when it is free, so
		// sign-in works even if GitHub matched loopback redirect ports exactly.
		preferred = app.RegisteredPort
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
	sy := syncer.New(syncer.Options{
		Store: st, Provider: github.NewProvider(auth), Plan: syncPlan(cfg.Sync), Log: log,
		OnUpdate: func(events []store.Event, unread int) {
			popups := events
			if group.Load() {
				popups = groupRepeats(events)
			}
			tray.NotifyEvents(popups) // own popups, filtered by notify.Prefs (Tray.SetPrefs)
			setBadge(int64(unread))
			publishLive(srv, live.Load().Filter(events, time.Now()))
		},
	})
	// Settings apply live: popups, toasts, poll interval.
	cfgs.Subscribe(func(_, cur config.Settings) {
		lp := liveFilter(cur.Notifications)
		live.Store(&lp)
		group.Store(cur.Notifications.Group)
		sy.SetPlan(syncPlan(cur.Sync))
		if tray != nil {
			tray.SetPrefs(notifyPrefs(cur.Notifications))
			tray.SetTheme(popupTheme(cur.Appearance))
		}
	})
	var testN atomic.Int64

	srv, err := api.New(context.Background(), api.Options{
		Assets:         issuewatcher.Assets(),
		PreferredPort:  preferred,
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
			tray.ShowCards(c)
			log.Info("test notification shown", "item", c.ItemID)
		},
		// Tray/notification clicks give this process foreground rights: bring the
		// dashboard's browser window to the front, or open a new tab.
		Focus: notify.FocusDashboard,
	})
	if err != nil {
		return err
	}
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
		icon, err := notify.TrayIcon(int(n))
		if err == nil {
			err = tray.SetIcon(icon)
		}
		log.Info("tray badge set", "count", n, "label", notify.BadgeLabel(int(n)), "err", err)
	}

	syncCtx, stopSync := context.WithCancel(context.Background())
	defer stopSync()

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
		go sy.Run(syncCtx)
		if minimized {
			log.Info("started minimized: dashboard not opened")
		} else {
			srv.OpenBrowser("/")
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
	f, err := os.OpenFile(filepath.Join(logDir, "issuewatcher.log"), //nolint:gosec // G304: fixed name inside our own data dir
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
	h := slog.NewTextHandler(io.MultiWriter(f, os.Stderr), &slog.HandlerOptions{Level: level})
	return slog.New(h), func() { _ = f.Close() }, nil
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
