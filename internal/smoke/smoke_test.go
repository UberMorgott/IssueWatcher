package smoke

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
)

// The test binary doubles as a fake factorio.exe (FAKE_FACTORIO=mode).
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_FACTORIO"); mode != "" {
		os.Exit(fakeFactorio(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeFactorio mimics the flags the adapter uses; it appends its argv to
// $FAKE_FACTORIO_ARGS and writes factorio-current.log into write-data.
func fakeFactorio(mode string, args []string) int {
	if f := os.Getenv("FAKE_FACTORIO_ARGS"); f != "" {
		b, _ := json.Marshal(args)
		fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test file
		_, _ = fh.Write(append(b, '\n'))
		_ = fh.Close()
	}
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	switch {
	case slices.Contains(args, "--version"):
		fmt.Println("Version: 2.0.77 (build 84539, win64, steam)")
		return 0
	case slices.Contains(args, "--help"):
		if mode != "noflags" {
			fmt.Println("  -c, --config PATH  config file\n      --mod-directory PATH  Mod directory\n      --create FILE  create a new map\n" +
				"      --benchmark FILE  load save and run benchmark\n      --benchmark-ticks N  number of ticks")
		}
		return 0
	}
	cfg, _ := os.ReadFile(flag("--config"))
	write := ""
	for l := range strings.SplitSeq(string(cfg), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "write-data="); ok {
			write = filepath.FromSlash(v)
		}
	}
	logf := func(s string) { _ = os.WriteFile(filepath.Join(write, "factorio-current.log"), []byte(s), 0o600) }
	if _, err := os.Stat(filepath.Join(flag("--mod-directory"), "mod-list.json")); err != nil {
		fmt.Println("Error: no mod-list.json")
		return 2
	}
	if f := flag("--create"); f != "" {
		if mode == "data" {
			fmt.Println("------------- Error -------------\nFailed to load mod \"my-mod\": __my-mod__/data.lua:1: boom data")
			logf("   0.1 Error ModManager.cpp:1: boom\n")
			return 1
		}
		logf("   0.5 Loading mod my-mod 1.0.1 (data.lua)\n   0.9 Goodbye\n")
		_ = os.WriteFile(f, []byte("map"), 0o600)
		return 0
	}
	if f := flag("--benchmark"); f != "" {
		if _, err := os.Stat(f); err != nil {
			fmt.Println("Error: no save " + f)
			return 1
		}
		if mode == "logerr" {
			logf("   1.0 Error while running event my-mod::on_tick (ID 0)\n__my-mod__/control.lua:3: boom tick\n")
			return 0
		}
		fmt.Printf("  Performed %s updates in 61.698 ms\n", flag("--benchmark-ticks"))
		logf("   1.0 Goodbye\n")
		return 0
	}
	return 3
}

// execFake runs argv with the fake's environment (no job object in tests).
func execFake(ctx context.Context, dir string, argv []string, _ string, timeout time.Duration) CmdResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the test binary
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	r := CmdResult{Command: strings.Join(argv, " "), OK: err == nil, Output: out.String()}
	if cmd.ProcessState != nil {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	return r
}

func zipMod(t *testing.T, path, top string, info map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(top + "/info.json")
	b, _ := json.Marshal(info)
	_, _ = w.Write(b)
	w, _ = zw.Create(top + "/control.lua")
	_, _ = w.Write([]byte("-- mod\n"))
	_ = zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

type fenv struct {
	runner  Runner
	req     Request
	owner   string // the owner's mods folder
	argsLog string
}

func newFactorio(t *testing.T, mode string) *fenv {
	t.Helper()
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_FACTORIO", mode)
	f := &fenv{owner: filepath.Join(root, "owner", "mods"), argsLog: filepath.Join(root, "args.log")}
	t.Setenv("FAKE_FACTORIO_ARGS", f.argsLog)
	zipMod(t, filepath.Join(f.owner, "dep-a_1.0.0.zip"), "dep-a_1.0.0", map[string]any{"name": "dep-a", "version": "1.0.0"})
	zipMod(t, filepath.Join(f.owner, "dep-a_1.10.0.zip"), "dep-a_1.10.0", map[string]any{"name": "dep-a", "version": "1.10.0"})
	zipMod(t, filepath.Join(f.owner, "dep-a_1.9.0.zip"), "dep-a_1.9.0", map[string]any{"name": "dep-a", "version": "1.9.0"})
	depB := filepath.Join(f.owner, "dep-b_0.1.0")
	_ = os.MkdirAll(depB, 0o750)
	_ = os.WriteFile(filepath.Join(depB, "info.json"), []byte(`{"name":"dep-b","version":"0.1.0","dependencies":["base","dep-c >= 1.0"]}`), 0o600)
	zipMod(t, filepath.Join(f.owner, "dep-c_1.0.0.zip"), "dep-c_1.0.0", map[string]any{"name": "dep-c", "version": "1.0.0"})
	zipMod(t, filepath.Join(f.owner, "unrelated_1.0.0.zip"), "unrelated_1.0.0", map[string]any{"name": "unrelated", "version": "1.0.0"})
	archive := filepath.Join(root, "artifact", "my-mod_1.0.1.zip")
	zipMod(t, archive, "my-mod_1.0.1", map[string]any{"name": "my-mod", "version": "1.0.1",
		"dependencies": []string{"base >= 2.0", "dep-a", "? optional-mod", "(?) hidden-opt", "! bad-mod", "~ dep-b", "space-age"}})
	f.runner = Runner{Exec: execFake, Detect: func() (Install, error) {
		return Install{Exe: exe, Data: filepath.Join(root, "game", "data"), Mods: f.owner}, nil
	}, Timeout: time.Minute}
	f.req = Request{Profile: config.SmokeProfile{Kind: config.SmokeFactorio, Ticks: 120}, Archive: archive, Name: "my-mod", Version: "1.0.1",
		WorkDir: filepath.Join(root, "work"), LogDir: filepath.Join(root, "logs")}
	return f
}

func (f *fenv) calls(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(f.argsLog)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var a []string
		_ = json.Unmarshal([]byte(l), &a)
		out = append(out, a)
	}
	return out
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestFactorioSmokePasses(t *testing.T) {
	f := newFactorio(t, "ok")
	ownerBefore := listDir(t, f.owner)
	res, err := f.runner.Run(t.Context(), f.req)
	if err != nil || !res.OK || res.Kind != config.SmokeFactorio || res.Version != "2.0.77" || len(res.Steps) != 2 ||
		res.Steps[0].Command != "factorio create" || res.Steps[1].Command != "factorio benchmark" || len(res.Errors) != 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if !strings.Contains(res.Steps[1].Output, "Performed 120 updates") {
		t.Fatalf("benchmark output %q", res.Steps[1].Output)
	}
	// Temp mod dir: the archive, the required dependencies (newest version,
	// recursively), never optional / incompatible / unrelated ones.
	mods := filepath.Join(f.req.WorkDir, "mods")
	got := listDir(t, mods)
	want := []string{"dep-a_1.10.0.zip", "dep-b_0.1.0", "dep-c_1.0.0.zip", "mod-list.json", "my-mod_1.0.1.zip"}
	if !slices.Equal(got, want) {
		t.Fatalf("mod dir %v, want %v", got, want)
	}
	var list struct {
		Mods []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"mods"`
	}
	b, _ := os.ReadFile(filepath.Join(mods, "mod-list.json")) //nolint:gosec // test file
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	en := map[string]bool{}
	for _, m := range list.Mods {
		en[m.Name] = m.Enabled
	}
	if !en["base"] || !en["my-mod"] || !en["dep-a"] || !en["dep-b"] || !en["dep-c"] || !en["space-age"] || en["quality"] || en["elevated-rails"] ||
		len(en) != 8 {
		t.Fatalf("mod-list %v", en)
	}
	// Isolation: own config with write-data in the work dir; the owner's mods folder is untouched.
	calls := f.calls(t)
	if len(calls) != 4 || calls[0][0] != "--version" || calls[1][0] != "--help" {
		t.Fatalf("calls %v", calls)
	}
	create, bench := calls[2], calls[3]
	if create[slices.Index(create, "--mod-directory")+1] != mods || !slices.Contains(create, "--disable-audio") ||
		create[slices.Index(create, "--config")+1] != filepath.Join(f.req.WorkDir, "config.ini") ||
		bench[slices.Index(bench, "--benchmark")+1] != create[slices.Index(create, "--create")+1] ||
		bench[slices.Index(bench, "--benchmark-ticks")+1] != "120" {
		t.Fatalf("create %v bench %v", create, bench)
	}
	ini, _ := os.ReadFile(filepath.Join(f.req.WorkDir, "config.ini"))
	if !strings.Contains(string(ini), "write-data="+filepath.ToSlash(filepath.Join(f.req.WorkDir, "write"))) {
		t.Fatalf("config.ini %s", ini)
	}
	if after := listDir(t, f.owner); !slices.Equal(after, ownerBefore) {
		t.Fatalf("owner mods folder changed: %v", after)
	}
}

// The profile's save is benchmarked from a copy; default ticks 600.
func TestFactorioSmokeLoadsSaveCopy(t *testing.T) {
	f := newFactorio(t, "ok")
	save := filepath.Join(t.TempDir(), "test.zip")
	if err := os.WriteFile(save, []byte("save"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.req.Profile.Save, f.req.Profile.Ticks = save, 0
	res, err := f.runner.Run(t.Context(), f.req)
	if err != nil || !res.OK {
		t.Fatalf("res %+v err %v", res, err)
	}
	bench := f.calls(t)[3]
	if p := bench[slices.Index(bench, "--benchmark")+1]; p == save || filepath.Dir(p) != f.req.WorkDir ||
		bench[slices.Index(bench, "--benchmark-ticks")+1] != "600" {
		t.Fatalf("bench %v", bench)
	}
}

func TestFactorioSmokeFailures(t *testing.T) {
	// Data stage error: create fails, no benchmark.
	f := newFactorio(t, "data")
	res, err := f.runner.Run(t.Context(), f.req)
	if err != nil || res.OK || len(res.Steps) != 1 || res.Steps[0].ExitCode != 1 ||
		!slices.ContainsFunc(res.Errors, func(s string) bool { return strings.Contains(s, `Failed to load mod "my-mod"`) }) ||
		!strings.Contains(res.Summary(), "factorio create: exit 1") {
		t.Fatalf("data error %+v %v (summary %q)", res, err, res.Summary())
	}
	// Control stage error only in the log, exit 0: still a failure.
	f = newFactorio(t, "logerr")
	res, err = f.runner.Run(t.Context(), f.req)
	if err != nil || res.OK || len(res.Steps) != 2 || res.Steps[1].OK ||
		!slices.ContainsFunc(res.Errors, func(s string) bool { return strings.Contains(s, "__my-mod__/control.lua:3: boom tick") }) {
		t.Fatalf("log error %+v %v", res, err)
	}
}

func TestFactorioSmokeUnavailable(t *testing.T) {
	f := newFactorio(t, "noflags")
	if _, err := f.runner.Run(t.Context(), f.req); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("no flags: %v", err)
	}
	f = newFactorio(t, "ok")
	if err := os.Remove(filepath.Join(f.owner, "dep-c_1.0.0.zip")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Run(t.Context(), f.req); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "dependency dep-c") {
		t.Fatalf("missing dependency: %v", err)
	}
	f = newFactorio(t, "ok")
	f.req.Profile.Install = filepath.Join(t.TempDir(), "nope")
	if _, err := f.runner.Run(t.Context(), f.req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bad install override: %v", err)
	}
	f.req.Profile.Install, f.runner.Detect = "", func() (Install, error) { return Install{}, fmt.Errorf("%w: none", ErrUnavailable) }
	if _, err := f.runner.Run(t.Context(), f.req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no install: %v", err)
	}
	if _, err := (Runner{}).Run(t.Context(), Request{Profile: config.SmokeProfile{Kind: "none"}, WorkDir: t.TempDir()}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("kind none: %v", err)
	}
}

// InstallAt reads the install's config-path.cfg → config.ini for data and mods.
func TestInstallAt(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "bin", "x64", "factorio.exe")
	_ = os.MkdirAll(filepath.Dir(exe), 0o750)
	_ = os.WriteFile(exe, nil, 0o600)
	_ = os.MkdirAll(filepath.Join(root, "config"), 0o750)
	_ = os.WriteFile(filepath.Join(root, "config-path.cfg"), []byte("config-path=__PATH__executable__/../../config\nuse-system-read-write-data-directories=false\n"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "config", "config.ini"), []byte("; c\n[path]\nread-data=__PATH__executable__/../../data\nwrite-data=__PATH__executable__/../..\n[other]\nwrite-data=X\n"), 0o600)
	in := InstallAt(exe)
	if in.Exe != exe || in.Data != filepath.Join(root, "data") || in.Mods != filepath.Join(root, "mods") {
		t.Fatalf("install %+v", in)
	}
	// System write-data (installer builds).
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	_ = os.WriteFile(filepath.Join(root, "config", "config.ini"), []byte("[path]\nwrite-data=__PATH__system-write-data__\n"), 0o600)
	if in := InstallAt(exe); in.Mods != filepath.Join(root, "appdata", "Factorio", "mods") {
		t.Fatalf("system write-data %+v", in)
	}
	// The override accepts the install root.
	got, err := (Runner{}).install(root)
	if err != nil || got.Exe != exe {
		t.Fatalf("override %+v %v", got, err)
	}
}

func TestCommandSmoke(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "a", "my-mod_1.0.1.zip")
	zipMod(t, archive, "my-mod_1.0.1", map[string]any{"name": "my-mod", "version": "1.0.1"})
	var gotDir, gotLine string
	r := Runner{Shell: func(_ context.Context, d, line, _ string, timeout time.Duration) CmdResult {
		gotDir, gotLine = d, line
		if timeout <= 0 {
			return CmdResult{ExitCode: -1}
		}
		ok := true
		if _, err := os.Stat(filepath.Join(d, "my-mod_1.0.1.zip")); err != nil {
			ok = false
		}
		return CmdResult{Command: line, OK: ok, Output: strings.Repeat("x", 5000)}
	}}
	work := filepath.Join(dir, "work")
	req := Request{Profile: config.SmokeProfile{Kind: config.SmokeCommand, Command: "check.cmd {archive} {name} {version}"},
		Archive: archive, Name: "my-mod", Version: "1.0.1", WorkDir: work}
	res, err := r.Run(t.Context(), req)
	if err != nil || !res.OK || gotDir != work || len(res.Steps) != 1 || len(res.Steps[0].Output) > 4010 ||
		gotLine != `check.cmd "`+filepath.Join(work, "my-mod_1.0.1.zip")+`" my-mod 1.0.1` {
		t.Fatalf("res %+v err %v line %q", res, err, gotLine)
	}
	r.Shell = func(context.Context, string, string, string, time.Duration) CmdResult {
		return CmdResult{Command: "check.cmd", ExitCode: 2, Output: "mod failed to load"}
	}
	res, err = r.Run(t.Context(), req)
	if err != nil || res.OK || res.Summary() != "check.cmd: exit 2: mod failed to load" {
		t.Fatalf("failing command %+v %v %q", res, err, res.Summary())
	}
}

// errorLines on real Factorio 2.0 output.
func TestErrorLines(t *testing.T) {
	out := `   0.030 Info ModManager.cpp:449: FeatureFlag quality = false
   0.054 Loading mod auto-build-and-deconstruct 1.1.6 (data.lua)
   1.234 Script @__lazy__/control.lua:10: debug message
------------- Error -------------
Failed to load mod "bad": __bad__/data.lua:1: boom data
	[C]: in function 'error'
	__bad__/data.lua:1: in main chunk
The mod bad (0.0.1) caused a non-recoverable error.
Error while running event bad::on_tick (ID 0)
   2.000 Error MainLoop.cpp:1: something
  Performed 600 updates in 61.698 ms`
	got := errorLines(out)
	want := []string{"Failed to load mod \"bad\": __bad__/data.lua:1: boom data", "__bad__/data.lua:1: in main chunk",
		"The mod bad (0.0.1) caused a non-recoverable error.", "Error while running event bad::on_tick (ID 0)", "2.000 Error MainLoop.cpp:1: something"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

// One real run on the owner's install (opt-in): IW_REAL_FACTORIO_ARCHIVE = a
// built mod archive; IW_REAL_FACTORIO_SAVE = optional save.
func TestFactorioRealInstall(t *testing.T) {
	archive := os.Getenv("IW_REAL_FACTORIO_ARCHIVE")
	if archive == "" {
		t.Skip("set IW_REAL_FACTORIO_ARCHIVE to run against the local Factorio install")
	}
	in, err := DetectFactorio()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("install %+v", in)
	info, err := zipInfo(archive)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	res, err := Runner{Exec: execFake}.Run(t.Context(), Request{
		Profile: config.SmokeProfile{Kind: config.SmokeFactorio, Save: os.Getenv("IW_REAL_FACTORIO_SAVE")},
		Archive: archive, Name: info.Name, Version: info.Version, WorkDir: filepath.Join(work, "w"), LogDir: filepath.Join(work, "logs")})
	for _, s := range res.Steps {
		t.Logf("%s ok=%v exit=%d\n%s", s.Command, s.OK, s.ExitCode, tail(s.Output, 600))
	}
	t.Logf("ok=%v version=%s errors=%q duration=%dms", res.OK, res.Version, res.Errors, res.DurationMS)
	if err != nil || !res.OK {
		t.Fatalf("real smoke failed: %v %s", err, res.Summary())
	}
}
