package steamugc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/secret"
	"github.com/UberMorgott/issuewatcher/internal/tools"
)

// ErrCreateUnknown: an earlier creation was started and its answer is lost:
// the owner checks his Workshop files, then ResetCreate. ErrNoSteamAPI: no
// usable steam_api64.dll was found.
var (
	ErrCreateUnknown = errors.New("steam: an earlier Workshop item creation has no recorded answer")
	ErrNoSteamAPI    = errors.New("steam: steam_api64.dll is not set up: no installed Steam game has one (Steamworks SDK 1.57+) to copy into tools\\steamworks")
	ErrSteam         = errors.New("steam: the Steam client refused")
	// ErrSteamOffline: the Steam client is not running or not signed in
	// (API init failed, BLoggedOn false or EResult 21): nothing was sent.
	ErrSteamOffline = fmt.Errorf("steam: the Steam client is not running or not signed in — start Steam and sign in as the item's owner (Workshop uploads use the running Steam client): %w", provider.ErrNoUploadAuth)
	// ErrUploadUnknown: the submit was sent and its answer is lost.
	ErrUploadUnknown = errors.New("steam: the Workshop upload's outcome is unknown")
	errNoAnswer      = errors.New("no answer")
)

const itemsFile = "steam-items.json"

// Options configures the Client.
type Options struct {
	DataDir string // data dir: secrets\steam-items.json, tools\steamugc, tools\steamworks
	Exe     string // the helper binary (default: this executable)
	// Find locates a steam_api64.dll to copy into tools\steamworks once
	// (default: the newest usable one of the installed Steam games; tests: a fake).
	Find func() (string, error)
	// Spawn runs the helper in dir (tests: a fake); default exec with a
	// timeout. The helper's stderr (upload progress lines) goes to stderr.
	Spawn func(ctx context.Context, exe, dir string, env []string, stdin []byte, stderr io.Writer) ([]byte, error)
	Now   func() time.Time
}

// Client creates Workshop items and writes their pages through the helper.
type Client struct {
	opts Options
	mu   sync.Mutex
	pmu  sync.Mutex     // provisioning steam_api64.dll
	prog tools.Progress // its status
}

// New returns the client.
func New(opts Options) *Client {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Spawn == nil {
		opts.Spawn = spawn
	}
	if opts.Find == nil {
		opts.Find = findDLL
	}
	return &Client{opts: opts}
}

// spawnTimeout backstops the helper (its own calls time out first: 10 min,
// an upload 2 h).
const spawnTimeout = 2*time.Hour + 10*time.Minute

func spawn(ctx context.Context, exe, dir string, env []string, stdin []byte, stderr io.Writer) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, spawnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, HelperArg)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Dir = dir
	hideWindow(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr
	err := cmd.Run()
	return out.Bytes(), err
}

// progressLines feeds the helper's stderr JSON lines to a progress func.
type progressLines struct {
	buf []byte
	f   func(UploadProgress)
}

func (p *progressLines) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		var up UploadProgress
		if json.Unmarshal(bytes.TrimSpace(p.buf[:i]), &up) == nil && p.f != nil {
			p.f(up)
		}
		p.buf = p.buf[i+1:]
	}
	if len(p.buf) > 64<<10 {
		p.buf = p.buf[:0]
	}
	return len(b), nil
}

// item is one entry of the items journal: State creating until Item is known.
type item struct {
	AppID      uint32    `json:"appId"`
	Item       uint64    `json:"item,omitempty"`
	State      string    `json:"state"` // creating | created
	StartedAt  time.Time `json:"startedAt"`
	ResultPath string    `json:"resultPath,omitempty"`
}

func (c *Client) itemsPath() string { return filepath.Join(c.opts.DataDir, "secrets", itemsFile) }

func (c *Client) items() (map[string]item, error) {
	m := map[string]item{}
	err := secret.ReadJSON(c.itemsPath(), &m)
	if errors.Is(err, secret.ErrNotFound) {
		return map[string]item{}, nil
	}
	return m, err
}

func (c *Client) saveItems(m map[string]item) error { return secret.WriteJSON(c.itemsPath(), m) }

func (c *Client) exe() (string, error) {
	if c.opts.Exe != "" {
		return c.opts.Exe, nil
	}
	return os.Executable()
}

// run sends job to the helper. A helper that dies without an answer is an error with no Result.
func (c *Client) run(ctx context.Context, job Job) (Result, error) {
	return c.runProgress(ctx, job, nil)
}

// runProgress is run with the helper's upload progress fed to progress. A
// helper that answers nothing is errNoAnswer.
func (c *Client) runProgress(ctx context.Context, job Job, progress func(UploadProgress)) (Result, error) {
	dll, err := c.steamAPI()
	if err != nil {
		return Result{}, err
	}
	exe, err := c.exe()
	if err != nil {
		return Result{}, err
	}
	in, _ := json.Marshal(job)
	app := strconv.FormatUint(uint64(job.AppID), 10)
	out, runErr := c.opts.Spawn(ctx, exe, filepath.Dir(dll), []string{EnvDLL + "=" + dll, "SteamAppId=" + app, "SteamGameId=" + app}, in, &progressLines{f: progress})
	var res Result
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		if runErr == nil {
			return Result{}, fmt.Errorf("steam helper: %w", errNoAnswer)
		}
		return Result{}, fmt.Errorf("steam helper: %w (%w)", errNoAnswer, runErr)
	}
	return res, nil
}

// Created is a Create outcome.
type Created struct {
	Item       uint64 `json:"item"`
	URL        string `json:"url,omitempty"`
	Created    bool   `json:"created"` // false: the item recorded earlier
	NeedsLegal bool   `json:"needsLegalAgreement,omitempty"`
	DryRun     bool   `json:"dryRun,omitempty"`
	Plan       string `json:"plan,omitempty"`
}

// ItemURL is the item's page.
func ItemURL(id uint64) string {
	return "https://steamcommunity.com/sharedfiles/filedetails/?id=" + strconv.FormatUint(id, 10)
}

// Item reports the recorded item of key (0 = none; creating = started, no answer).
func (c *Client) Item(key string) (uint64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.items()
	if err != nil {
		return 0, false, err
	}
	e := m[key]
	return e.Item, e.State == "creating" && e.Item == 0, nil
}

// adopt reads an answer the helper wrote before the app lost it.
func adopt(e item) uint64 {
	var r struct {
		Item uint64 `json:"item"`
	}
	b, err := os.ReadFile(e.ResultPath)
	if err != nil || json.Unmarshal(b, &r) != nil {
		return 0
	}
	return r.Item
}

// Create makes the Workshop item of key (one per key, ever): the journal
// entry is written before the helper runs, the helper writes the new id to
// its result file before anything else, and a retry reuses whatever was
// recorded — an entry without an id refuses (ErrCreateUnknown) instead of
// creating a second item.
func (c *Client) Create(ctx context.Context, key string, appID uint32, dryRun bool) (Created, error) {
	if key == "" || appID == 0 {
		return Created{}, fmt.Errorf("%w: project and app id", ErrBadPage)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.items()
	if err != nil {
		return Created{}, err
	}
	if e, ok := m[key]; ok {
		if e.Item == 0 {
			e.Item = adopt(e)
		}
		if e.Item != 0 {
			if e.State != "created" {
				e.State = "created"
				m[key] = e
				if err := c.saveItems(m); err != nil {
					return Created{}, err
				}
			}
			return Created{Item: e.Item, URL: ItemURL(e.Item)}, nil
		}
		return Created{}, fmt.Errorf("%w: started %s for %s (app %d); check your Workshop files at https://steamcommunity.com/my/myworkshopfiles/?appid=%d — if no new item is there, reset the creation and create again",
			ErrCreateUnknown, e.StartedAt.UTC().Format(time.RFC3339), key, e.AppID, e.AppID)
	}
	if dryRun {
		derr := c.canProvision()
		plan := fmt.Sprintf("ISteamUGC::CreateItem(app %d, community) through the running Steam client, sent once; the new id is recorded before anything else", appID)
		if derr != nil {
			plan += "; not ready: " + derr.Error()
		}
		return Created{DryRun: true, Plan: plan}, nil
	}
	dir := filepath.Join(c.opts.DataDir, "tools", "steamugc")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Created{}, err
	}
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	e := item{AppID: appID, State: "creating", StartedAt: c.opts.Now().UTC(), ResultPath: filepath.Join(dir, "create-"+hex.EncodeToString(nonce)+".json")}
	m[key] = e
	if err := c.saveItems(m); err != nil {
		return Created{}, err
	}
	res, err := c.run(context.WithoutCancel(ctx), Job{Op: "create", AppID: appID, ResultPath: e.ResultPath})
	id := res.Item
	if id == 0 {
		id = adopt(e)
	}
	if id != 0 {
		e.Item, e.State = id, "created"
		m[key] = e
		if serr := c.saveItems(m); serr != nil {
			return Created{}, fmt.Errorf("created item %d but could not record it: %w", id, serr)
		}
		return Created{Item: id, URL: ItemURL(id), Created: true, NeedsLegal: res.NeedsLegal}, nil
	}
	if err == nil && (res.Code == "init" || res.Code == "create" || res.Code == "bad_job") {
		delete(m, key) // definite: nothing was created
		_ = c.saveItems(m)
		return Created{}, fmt.Errorf("%w: %s", ErrSteam, res.Error)
	}
	if err == nil {
		err = errors.New(res.Error)
	}
	return Created{}, fmt.Errorf("%w (%w)", ErrCreateUnknown, err)
}

// ResetCreate drops an unanswered creation of key (the owner checked that no
// item was created). A recorded item is never dropped.
func (c *Client) ResetCreate(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, err := c.items()
	if err != nil {
		return err
	}
	e, ok := m[key]
	if !ok {
		return nil
	}
	if e.Item != 0 || adopt(e) != 0 {
		return fmt.Errorf("steam: %s has item %d recorded; it is not dropped", key, max(e.Item, adopt(e)))
	}
	delete(m, key)
	return c.saveItems(m)
}

// PageResult is a SetPage outcome (or its plan).
type PageResult struct {
	DryRun     bool         `json:"dryRun,omitempty"`
	Item       uint64       `json:"item"`
	URL        string       `json:"url"`
	Languages  []LangResult `json:"languages,omitempty"`
	Plan       []PlanText   `json:"plan,omitempty"`
	Preview    string       `json:"preview,omitempty"`
	Tags       []string     `json:"tags,omitempty"`
	Visibility string       `json:"visibility,omitempty"`
	Sources    []string     `json:"sources,omitempty"`
	NeedsLegal bool         `json:"needsLegalAgreement,omitempty"`
}

// PlanText is one language of a dry run.
type PlanText struct {
	Language string `json:"language"`
	Title    string `json:"title"`
	Chars    int    `json:"descriptionChars"`
}

func visibilityName(v int) string {
	for k, n := range Visibilities {
		if n == v {
			return k
		}
	}
	return ""
}

// SetPage writes the page: one submit per language (english first, with the
// preview, tags and visibility). Re-sending the same page is harmless.
func (c *Client) SetPage(ctx context.Context, appID uint32, id uint64, p Page, changeNote string, dryRun bool) (PageResult, error) {
	if appID == 0 || id == 0 || len(p.Texts) == 0 {
		return PageResult{}, fmt.Errorf("%w: app id, item and texts", ErrBadPage)
	}
	out := PageResult{DryRun: dryRun, Item: id, URL: ItemURL(id), Preview: p.Preview, Tags: p.Tags, Visibility: visibilityName(p.Visibility), Sources: p.Sources}
	if dryRun {
		for _, t := range p.Texts {
			out.Plan = append(out.Plan, PlanText{Language: t.Language, Title: t.Title, Chars: len([]rune(t.Description))})
		}
		if err := c.canProvision(); err != nil {
			return out, err
		}
		return out, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	res, err := c.run(ctx, Job{Op: "page", AppID: appID, Item: id, Page: p, ChangeNote: changeNote})
	if err != nil {
		return out, err
	}
	out.Languages, out.NeedsLegal = res.Languages, res.NeedsLegal
	if res.Error != "" {
		return out, fmt.Errorf("%w: %s", ErrSteam, res.Error)
	}
	return out, nil
}

// WhoAmI starts the API once (no writes): the signed-in SteamID.
func (c *Client) WhoAmI(ctx context.Context, appID uint32) (string, error) {
	res, err := c.run(ctx, Job{Op: "whoami", AppID: appID})
	if err != nil {
		return "", err
	}
	if res.Error != "" {
		return "", fmt.Errorf("%w: %s", ErrSteam, res.Error)
	}
	return res.SteamID, nil
}

// UploadResult is an Upload outcome.
type UploadResult struct {
	Item       uint64 `json:"item"`
	EResult    int    `json:"eresult,omitempty"`
	NeedsLegal bool   `json:"needsLegalAgreement,omitempty"`
}

// Upload sends dir as item's new content with the change note, through the
// running Steam client (no login of its own). Errors: ErrSteamOffline (Steam
// not running / not signed in: nothing sent), ErrSteam (refused: nothing
// published), ErrUploadUnknown (sent, answer lost: probe before retrying),
// ErrNoSteamAPI / provisioning errors (nothing sent).
func (c *Client) Upload(ctx context.Context, appID uint32, item uint64, dir, note string, progress func(UploadProgress)) (UploadResult, error) {
	if appID == 0 || item == 0 || dir == "" || !filepath.IsAbs(dir) {
		return UploadResult{}, fmt.Errorf("%w: app id, item and an absolute content folder", ErrBadPage)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	res, err := c.runProgress(context.WithoutCancel(ctx), Job{Op: "upload", AppID: appID, Item: item, Content: dir, ChangeNote: note}, progress)
	out := UploadResult{Item: item, EResult: res.EResult, NeedsLegal: res.NeedsLegal}
	switch {
	case errors.Is(err, errNoAnswer):
		return out, fmt.Errorf("%w (%w)", ErrUploadUnknown, err)
	case err != nil:
		return out, err
	case res.Error == "":
		return out, nil
	case res.Code == "init" || res.Code == "not_logged_on":
		return out, fmt.Errorf("%w (%s)", ErrSteamOffline, res.Error)
	case res.Code == "unknown" || res.Sent && res.Code != "eresult":
		return out, fmt.Errorf("%w: %s", ErrUploadUnknown, res.Error)
	}
	return out, fmt.Errorf("%w: %s", ErrSteam, res.Error)
}

// Status is the running Steam client as the helper sees it.
type Status struct {
	Running  bool   `json:"running"`
	LoggedOn bool   `json:"loggedOn"`
	SteamID  string `json:"steamId,omitempty"`
	AppID    uint32 `json:"appId"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"` // init: the API did not start (Steam not running, …)
}

// Status starts the API once (no writes) as appID (the project's game; no
// default: Steam shows the owner playing that app): is the Steam client
// running and signed in, and as whom. A client that is not running is a
// Status, not an error.
func (c *Client) Status(ctx context.Context, appID uint32) (Status, error) {
	if appID == 0 {
		return Status{}, errors.New("steam: status needs the game's app id")
	}
	res, err := c.run(ctx, Job{Op: "status", AppID: appID})
	if err != nil {
		return Status{AppID: appID}, err
	}
	st := Status{Running: res.Running, LoggedOn: res.LoggedOn, AppID: appID, Error: res.Error, Code: res.Code}
	if res.Error == "" && res.SteamID != "0" {
		st.SteamID = res.SteamID
	}
	return st, nil
}

// Ready: steam_api64.dll is set up or can be (dry runs: nothing written).
func (c *Client) Ready() error { return c.canProvision() }

// Main is the helper process (main: os.Args[1] == HelperArg).
func Main() int {
	api, err := NewAPI(os.Getenv(EnvDLL))
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(Result{Error: err.Error(), Code: "init"})
		return 1
	}
	return HelperMain(api, os.Stdin, os.Stdout)
}
