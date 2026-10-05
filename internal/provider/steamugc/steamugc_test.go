package steamugc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/tools"
)

// The test binary doubles as the helper (os.Args[1] == HelperArg): with
// FAKE_UGC=<dir> it runs Run on a fake API that records its calls in
// <dir>/calls.jsonl; FAKE_UGC_MODE = ok | crash_after_create (answer
// recorded in the result file, the process dies before printing it) |
// crash_before_create | init_fail. Without FAKE_UGC it is the real helper.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		dir := os.Getenv("FAKE_UGC")
		if dir == "" {
			os.Exit(Main())
		}
		api := &fakeAPI{dir: dir, mode: os.Getenv("FAKE_UGC_MODE")}
		if api.mode == "crash_after_create" || api.mode == "crash_before_create" {
			var job Job
			_ = json.NewDecoder(os.Stdin).Decode(&job)
			if api.mode == "crash_after_create" {
				Run(api, job)
			}
			os.Exit(3)
		}
		os.Exit(HelperMain(api, os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

type fakeAPI struct {
	dir, mode string
}

func (f *fakeAPI) log(v any) {
	b, _ := json.Marshal(v)
	fl, err := os.OpenFile(filepath.Join(f.dir, "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fl.Write(append(b, '\n'))
	_ = fl.Close()
}

func (f *fakeAPI) Init() error {
	f.log(map[string]any{"call": "init", "appEnv": os.Getenv("SteamAppId"), "dll": os.Getenv(EnvDLL)})
	if f.mode == "init_fail" {
		return errors.New("Steam API init failed (2: no Steam)")
	}
	return nil
}
func (f *fakeAPI) Shutdown()       {}
func (f *fakeAPI) SteamID() uint64 { return 76561197996210591 }
func (f *fakeAPI) CreateItem(app uint32) (uint64, bool, int, error) {
	f.log(map[string]any{"call": "create", "app": app})
	return 3800000001, false, 1, nil
}
func (f *fakeAPI) Update(app uint32, item uint64, u Update) (int, bool, error) {
	f.log(map[string]any{"call": "update", "app": app, "item": item, "u": u})
	return 1, false, nil
}

type harness struct {
	c     *Client
	dir   string
	data  string
	mode  string
	src   string // the fake game's steam_api64.dll
	spawn atomic.Int32
	finds atomic.Int32
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{dir: t.TempDir(), data: t.TempDir(), mode: "ok"}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h.src = filepath.Join(t.TempDir(), "Game", "steam_api64.dll")
	if err := os.MkdirAll(filepath.Dir(h.src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.src, []byte("MZ fake steam_api64"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.c = New(Options{DataDir: h.data, Exe: exe,
		Find: func() (string, error) {
			h.finds.Add(1)
			if h.src == "" {
				return "", ErrNoSteamAPI
			}
			return h.src, nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) },
		Spawn: func(ctx context.Context, exe, dir string, env []string, stdin []byte) ([]byte, error) {
			h.spawn.Add(1)
			if dir != filepath.Join(h.data, "tools", "steamworks") {
				t.Errorf("helper runs in %s", dir)
			}
			return spawn(ctx, exe, dir, append(env, "FAKE_UGC="+h.dir, "FAKE_UGC_MODE="+h.mode), stdin)
		}})
	return h
}

func (h *harness) calls(t *testing.T) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(h.dir, "calls.jsonl"))
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

const key = "github:UberMorgott/PP-ContentTool"

func TestCreateOnceAndReuse(t *testing.T) {
	h := newHarness(t)
	dry, err := h.c.Create(context.Background(), key, 839770, true)
	if err != nil || !dry.DryRun || h.spawn.Load() != 0 {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	got, err := h.c.Create(context.Background(), key, 839770, false)
	if err != nil || got.Item != 3800000001 || !got.Created || got.URL != "https://steamcommunity.com/sharedfiles/filedetails/?id=3800000001" {
		t.Fatalf("create: %+v %v", got, err)
	}
	cs := h.calls(t)
	if len(cs) != 2 || cs[0]["appEnv"] != "839770" || cs[0]["dll"] != filepath.Join(h.data, "tools", "steamworks", "steam_api64.dll") || cs[1]["call"] != "create" {
		t.Fatalf("calls %v", cs)
	}
	again, err := h.c.Create(context.Background(), key, 839770, false)
	if err != nil || again.Item != 3800000001 || again.Created || h.spawn.Load() != 1 {
		t.Fatalf("second create: %+v %v spawns %d", again, err, h.spawn.Load())
	}
	if id, creating, _ := h.c.Item(key); id != 3800000001 || creating {
		t.Fatalf("item %d %v", id, creating)
	}
	if err := h.c.ResetCreate(key); err == nil {
		t.Fatal("reset dropped a recorded item")
	}
}

func TestCreateCrashAfterCreateAdoptsTheID(t *testing.T) {
	h := newHarness(t)
	h.mode = "crash_after_create"
	got, err := h.c.Create(context.Background(), key, 839770, false)
	if err != nil || got.Item != 3800000001 || !got.Created {
		t.Fatalf("crash after create: %+v %v", got, err)
	}
	h.mode = "ok"
	if again, err := h.c.Create(context.Background(), key, 839770, false); err != nil || again.Item != 3800000001 || h.spawn.Load() != 1 {
		t.Fatalf("retry: %+v %v", again, err)
	}
}

func TestCreateLostAnswerNeverCreatesTwice(t *testing.T) {
	h := newHarness(t)
	h.mode = "crash_before_create" // the helper died: did Steam create one? unknown
	if _, err := h.c.Create(context.Background(), key, 839770, false); !errors.Is(err, ErrCreateUnknown) {
		t.Fatalf("lost answer: %v", err)
	}
	h.mode = "ok"
	if _, err := h.c.Create(context.Background(), key, 839770, false); !errors.Is(err, ErrCreateUnknown) || h.spawn.Load() != 1 {
		t.Fatalf("retry must refuse: %v spawns %d", err, h.spawn.Load())
	}
	if _, creating, _ := h.c.Item(key); !creating {
		t.Fatal("not reported as creating")
	}
	if err := h.c.ResetCreate(key); err != nil {
		t.Fatal(err)
	}
	if got, err := h.c.Create(context.Background(), key, 839770, false); err != nil || got.Item == 0 {
		t.Fatalf("after reset: %+v %v", got, err)
	}
}

func TestCreateRefusedIsRetryable(t *testing.T) {
	h := newHarness(t)
	h.mode = "init_fail"
	if _, err := h.c.Create(context.Background(), key, 839770, false); !errors.Is(err, ErrSteam) || !strings.Contains(err.Error(), "no Steam") {
		t.Fatalf("init fail: %v", err)
	}
	h.mode = "ok"
	if got, err := h.c.Create(context.Background(), key, 839770, false); err != nil || !got.Created {
		t.Fatalf("retry: %+v %v", got, err)
	}
}

// writePage lays out a mod folder's page sources.
func writePage(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadPageAllLanguages(t *testing.T) {
	dir := writePage(t, map[string]string{
		"workshop/locale/description.russian.txt":  "\ufeff[h1]Свободная камера[/h1]\r\nТекст",
		"workshop/locale/description.english.txt":  "[h1]Free Camera[/h1]\nText",
		"workshop/locale/description.schinese.txt": "[h1]自由镜头[/h1]",
		"workshop/locale/title.russian.txt":        "Свободная камера",
		"workshop/locale/description.klingon.txt":  "ignored",
		"image/steam_preview.jpg":                  "JPEG",
	})
	p, err := LoadPage(dir, PageSpec{Title: "Free Camera", Tags: []string{"Tactical", "Gameplay", "Tactical"}, Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Text{{"english", "Free Camera", "[h1]Free Camera[/h1]\nText"}, {"schinese", "Free Camera", "[h1]自由镜头[/h1]"}, {"russian", "Свободная камера", "[h1]Свободная камера[/h1]\nТекст"}}
	if len(p.Texts) != 3 {
		t.Fatalf("texts %+v", p.Texts)
	}
	for i, w := range want {
		if p.Texts[i] != w {
			t.Fatalf("text %d: %+v want %+v", i, p.Texts[i], w)
		}
	}
	if p.Preview != filepath.Join(dir, "image", "steam_preview.jpg") || strings.Join(p.Tags, ",") != "Tactical,Gameplay" || p.Visibility != 0 {
		t.Fatalf("page %+v", p)
	}
	for _, c := range []struct {
		files map[string]string
		spec  PageSpec
		want  string
	}{
		{map[string]string{"workshop/locale/description.russian.txt": "x"}, PageSpec{Title: "T"}, "description.english.txt is missing"},
		{map[string]string{"workshop/locale/description.english.txt": "x"}, PageSpec{}, "no title for english"},
		{map[string]string{"workshop/locale/description.english.txt": "x"}, PageSpec{Title: "T", Visibility: "hidden"}, "visibility"},
		{map[string]string{"workshop/locale/description.english.txt": "x"}, PageSpec{Title: "T", LocaleDir: "../x"}, "inside the mod folder"},
		{map[string]string{"workshop/locale/description.english.txt": "x", "p.bmp": "x"}, PageSpec{Title: "T", Preview: "p.bmp"}, "jpg, png or gif"},
		{map[string]string{"workshop/locale/description.english.txt": "x", "big.jpg": strings.Repeat("x", maxPreview+1)}, PageSpec{Title: "T", Preview: "big.jpg"}, "at most 1 MB"},
	} {
		if _, err := LoadPage(writePage(t, c.files), c.spec); !errors.Is(err, ErrBadPage) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.spec, err, c.want)
		}
	}
}

func TestSetPageOneSubmitPerLanguage(t *testing.T) {
	h := newHarness(t)
	dir := writePage(t, map[string]string{
		"workshop/locale/description.english.txt": "EN",
		"workshop/locale/description.russian.txt": "RU",
		"workshop/image/steam_preview.jpg":        "JPEG",
	})
	p, err := LoadPage(dir, PageSpec{Title: "Content Tool", Tags: []string{"Tools"}, Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	dry, err := h.c.SetPage(context.Background(), 839770, 3800000001, p, "", true)
	if err != nil || len(dry.Plan) != 2 || h.spawn.Load() != 0 || dry.Visibility != "public" {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	res, err := h.c.SetPage(context.Background(), 839770, 3800000001, p, "Initial page", false)
	if err != nil || len(res.Languages) != 2 || !res.Languages[0].OK || !res.Languages[1].OK {
		t.Fatalf("set page: %+v %v", res, err)
	}
	cs := h.calls(t)
	if len(cs) != 3 {
		t.Fatalf("calls %v", cs)
	}
	en, ok1 := cs[1]["u"].(map[string]any)
	ru, ok2 := cs[2]["u"].(map[string]any)
	tags, ok3 := en["Tags"].([]any)
	if !ok1 || !ok2 || !ok3 || len(tags) != 1 {
		t.Fatalf("calls %v", cs)
	}
	if en["Language"] != "english" || en["Title"] != "Content Tool" || en["Description"] != "EN" || en["Preview"] != filepath.Join(dir, "workshop", "image", "steam_preview.jpg") ||
		en["Visibility"] != float64(0) || en["ChangeNote"] != "Initial page" || tags[0] != "Tools" {
		t.Fatalf("english update %v", en)
	}
	if ru["Language"] != "russian" || ru["Description"] != "RU" || ru["Preview"] != "" || ru["Tags"] != nil || ru["Visibility"] != float64(-1) {
		t.Fatalf("russian update %v", ru)
	}
}

// TestRealWhoAmI (opt-in: STEAMUGC_REAL=<appid>) starts the real Steam API
// once through the running Steam client: no writes.
func TestRealWhoAmI(t *testing.T) {
	app := os.Getenv("STEAMUGC_REAL")
	if app == "" {
		t.Skip("STEAMUGC_REAL not set")
	}
	exe, _ := os.Executable()
	data := t.TempDir()
	c := New(Options{DataDir: data, Exe: exe})
	dll, err := c.steamAPI()
	if err == nil && !strings.HasPrefix(dll, filepath.Join(data, "tools", "steamworks")) {
		t.Fatalf("dll %s outside the tools folder", dll)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dll %s", dll)
	id, err := strconv.ParseUint(app, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := c.WhoAmI(context.Background(), uint32(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("signed in as %s", sid)
}

// TestProvisionCopiesOnceAndChecksTheHash: the game's DLL is copied into
// tools\steamworks once (the source is not looked up again), a changed copy
// is replaced, and the helper only ever gets the tools copy.
func TestProvisionCopiesOnceAndChecksTheHash(t *testing.T) {
	h := newHarness(t)
	if st := h.c.Tool(); st.State != tools.Missing || st.Path != "" {
		t.Fatalf("before: %+v", st)
	}
	st, err := h.c.Provision(context.Background())
	dst := filepath.Join(h.data, "tools", "steamworks", "steam_api64.dll")
	if err != nil || st.State != tools.Ready || st.Path != dst || st.Source != h.src || st.At == "" {
		t.Fatalf("provision: %+v %v", st, err)
	}
	if b := h.toolFile(t); b != "MZ fake steam_api64" {
		t.Fatalf("copy %q", b)
	}
	if _, err := h.c.Provision(context.Background()); err != nil || h.finds.Load() != 1 {
		t.Fatalf("second provision: %v finds %d", err, h.finds.Load())
	}
	if _, err := h.c.WhoAmI(context.Background(), 839770); err != nil || h.finds.Load() != 1 {
		t.Fatalf("whoami: %v finds %d", err, h.finds.Load())
	}
	if err := os.WriteFile(dst, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := h.c.Tool(); st.State != tools.Missing {
		t.Fatalf("tampered copy counts: %+v", st)
	}
	if _, err := h.c.WhoAmI(context.Background(), 839770); err != nil || h.finds.Load() != 2 {
		t.Fatalf("recopy: %v finds %d", err, h.finds.Load())
	}
	if b := h.toolFile(t); b != "MZ fake steam_api64" {
		t.Fatalf("recopied %q", b)
	}
}

// TestProvisionNoSourceAndDryRunWritesNothing: without a game DLL the tool
// reports the error; a dry run plans without copying anything.
func TestProvisionNoSourceAndDryRunWritesNothing(t *testing.T) {
	h := newHarness(t)
	if dry, err := h.c.Create(context.Background(), key, 839770, true); err != nil || !dry.DryRun || strings.Contains(dry.Plan, "not ready") {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	if _, err := os.Stat(filepath.Join(h.data, "tools", "steamworks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run wrote the tools folder: %v", err)
	}
	h.src = ""
	if _, err := h.c.Provision(context.Background()); !errors.Is(err, ErrNoSteamAPI) {
		t.Fatalf("no source: %v", err)
	}
	if st := h.c.Tool(); st.State != tools.Failed || st.Error == "" {
		t.Fatalf("status after failure: %+v", st)
	}
	if _, err := h.c.WhoAmI(context.Background(), 839770); !errors.Is(err, ErrNoSteamAPI) || h.spawn.Load() != 0 {
		t.Fatalf("whoami without a DLL: %v spawns %d", err, h.spawn.Load())
	}
}

// toolFile is the app's copy of steam_api64.dll ("" = none).
func (h *harness) toolFile(t *testing.T) string {
	t.Helper()
	root, err := os.OpenRoot(h.data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	b, _ := root.ReadFile(filepath.Join("tools", "steamworks", "steam_api64.dll"))
	return string(b)
}
