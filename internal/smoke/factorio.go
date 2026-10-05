package smoke

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DefaultTicks is the benchmark length when the profile sets none.
const DefaultTicks = 600

// builtin mods ship with the game (data directory): enabled in mod-list.json
// only when the mod requires them, never copied.
var builtin = []string{"base", "core", "space-age", "quality", "elevated-rails"}

// requiredFlags must be in `factorio --help` of the installed version.
var requiredFlags = []string{"--config", "--mod-directory", "--create", "--benchmark", "--benchmark-ticks"}

// factorio loads the archive in the owner's install with a temp config
// (write-data in WorkDir: no log, lock or player data in the owner's
// folders), a temp --mod-directory holding the archive, its required
// dependencies copied from the owner's mods folder and a mod-list.json
// enabling exactly those; then `--create` (data stage + a new map) and
// `--benchmark` of the profile's save (or the new map) for N ticks (control
// stage, migrations). Pass = every exit code 0 and no mod error lines.
func (r Runner) factorio(ctx context.Context, req Request) (Result, error) {
	var res Result
	if r.Exec == nil {
		return res, fmt.Errorf("%w: processes cannot run here", ErrUnavailable)
	}
	inst, err := r.install(req.Profile.Install)
	if err != nil {
		return res, err
	}
	res.Install = inst.Exe
	work, logs := req.WorkDir, req.LogDir

	// The installed version and its flags (re-checked on every run).
	v := r.Exec(ctx, work, []string{inst.Exe, "--version"}, logs, remaining(ctx))
	if !v.OK {
		return res, fmt.Errorf("%w: %s --version: exit %d: %s", ErrUnavailable, inst.Exe, v.ExitCode, tail(v.Output, 500))
	}
	if m := versionLine.FindStringSubmatch(v.Output); m != nil {
		res.Version = m[1]
	}
	h := r.Exec(ctx, work, []string{inst.Exe, "--help"}, logs, remaining(ctx))
	for _, f := range requiredFlags {
		if !strings.Contains(h.Output, f+" ") {
			return res, fmt.Errorf("%w: Factorio %s has no %s flag", ErrUnavailable, res.Version, f)
		}
	}

	mods, write := filepath.Join(work, "mods"), filepath.Join(work, "write")
	for _, d := range []string{mods, write} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return res, err
		}
	}
	info, err := zipInfo(req.Archive)
	if err != nil {
		return res, fmt.Errorf("the archive: %w", err)
	}
	if err := copyFile(req.Archive, filepath.Join(mods, info.Name+"_"+info.Version+".zip")); err != nil {
		return res, err
	}
	enabled := map[string]bool{info.Name: true, "base": true}
	if err := r.copyDeps(info, inst.Mods, mods, enabled); err != nil {
		return res, err
	}
	if err := writeModList(mods, enabled); err != nil {
		return res, err
	}
	cfg := filepath.Join(work, "config.ini")
	ini := fmt.Sprintf("[path]\nread-data=%s\nwrite-data=%s\n", filepath.ToSlash(inst.Data), filepath.ToSlash(write))
	if err := os.WriteFile(cfg, []byte(ini), 0o600); err != nil {
		return res, err
	}
	base := []string{inst.Exe, "--config", cfg, "--mod-directory", mods, "--disable-audio"}

	created := filepath.Join(work, "smoke-map.zip")
	if !r.factorioStep(ctx, &res, work, logs, write, "create", append(slices.Clone(base), "--create", created)) {
		return res, nil
	}
	save := created
	if req.Profile.Save != "" {
		save = filepath.Join(work, "smoke-save.zip")
		if err := copyFile(req.Profile.Save, save); err != nil {
			return res, fmt.Errorf("%w: the save: %w", ErrUnavailable, err)
		}
	}
	ticks := req.Profile.Ticks
	if ticks <= 0 {
		ticks = DefaultTicks
	}
	r.factorioStep(ctx, &res, work, logs, write, "benchmark",
		append(slices.Clone(base), "--benchmark", save, "--benchmark-ticks", strconv.Itoa(ticks)))
	return res, nil
}

var versionLine = regexp.MustCompile(`(?m)^Version: (\d+\.\d+\.\d+)`)

// factorioStep runs one factorio process; false = it failed (res says why).
func (r Runner) factorioStep(ctx context.Context, res *Result, work, logs, write, label string, argv []string) bool {
	_ = os.Remove(filepath.Join(write, "factorio-current.log"))
	c := r.Exec(ctx, work, argv, logs, remaining(ctx))
	c.Command = "factorio " + label
	log, _ := os.ReadFile(filepath.Join(write, "factorio-current.log")) //nolint:gosec // our temp write-data dir
	errs := errorLines(c.Output + "\n" + string(log))
	c.Output = tail(c.Output, 3000)
	ok := c.OK && len(errs) == 0
	c.OK = ok
	res.Steps = append(res.Steps, c)
	res.Errors = append(res.Errors, errs...)
	res.OK = ok
	if !ok && len(errs) == 0 && c.ExitCode == 0 && !c.TimedOut {
		res.Note = label + " failed"
	}
	return ok
}

// errorRe matches Factorio's mod / script error lines.
var errorRe = regexp.MustCompile(`(?i)^\s*(?:\d+\.\d+\s+)?(?:Error\b.*|.*Failed to load mod.*|.*caused a non-recoverable error.*|.*Error while running.*|__[\w-]+__/.+:\d+: .+)`)

// errorLines returns the distinct error lines of out (at most 20).
func errorLines(out string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || !errorRe.MatchString(l) || slices.Contains(lines, l) {
			continue
		}
		lines = append(lines, tail(l, 300))
		if len(lines) == 20 {
			break
		}
	}
	return lines
}

// --- mods ------------------------------------------------------------------

// modInfo is the part of info.json the smoke test reads.
type modInfo struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Dependencies []string `json:"dependencies"`
}

// zipInfo reads <top>/info.json of a mod archive.
func zipInfo(path string) (modInfo, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return modInfo{}, err
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		parts := strings.Split(strings.TrimSuffix(f.Name, "/"), "/")
		if len(parts) != 2 || parts[1] != "info.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return modInfo{}, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		_ = rc.Close()
		if err != nil {
			return modInfo{}, err
		}
		return parseInfo(b)
	}
	return modInfo{}, errors.New("no <mod>/info.json in the archive")
}

func parseInfo(b []byte) (modInfo, error) {
	var m modInfo
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("info.json: %w", err)
	}
	if m.Name == "" || m.Version == "" {
		return m, errors.New("info.json: no name / version")
	}
	return m, nil
}

// required returns the names of the hard dependencies of a dependency list
// ("! x" incompatible, "? x" / "(?) x" optional are left out; "~ x" counts).
func required(deps []string) []string {
	var out []string
	for _, d := range deps {
		d = strings.TrimSpace(d)
		switch {
		case d == "", strings.HasPrefix(d, "!"), strings.HasPrefix(d, "?"), strings.HasPrefix(d, "(?)"):
			continue
		}
		d = strings.TrimSpace(strings.TrimPrefix(d, "~"))
		if f := strings.Fields(d); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

// copyDeps copies the required dependencies of m (recursively) from the
// owner's mods folder (read only) into mods and marks them enabled.
func (r Runner) copyDeps(m modInfo, owner, mods string, enabled map[string]bool) error {
	for _, name := range required(m.Dependencies) {
		if enabled[name] {
			continue
		}
		enabled[name] = true
		if slices.Contains(builtin, name) {
			continue
		}
		src, info, err := findMod(owner, name)
		if err != nil {
			return fmt.Errorf("%w: dependency %s: %w", ErrUnavailable, name, err)
		}
		dst := filepath.Join(mods, filepath.Base(src))
		if st, _ := os.Stat(src); st != nil && st.IsDir() {
			err = os.CopyFS(dst, os.DirFS(src))
		} else {
			err = copyFile(src, dst)
		}
		if err != nil {
			return fmt.Errorf("dependency %s: %w", name, err)
		}
		if err := r.copyDeps(info, owner, mods, enabled); err != nil {
			return err
		}
	}
	return nil
}

// findMod finds the newest installed copy of mod name in dir (name_X.Y.Z.zip,
// name_X.Y.Z/ or name/).
func findMod(dir, name string) (string, modInfo, error) {
	if dir == "" {
		return "", modInfo{}, errors.New("no mods folder of the Factorio install found")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", modInfo{}, err
	}
	best, bestInfo := "", modInfo{}
	for _, e := range entries {
		n := e.Name()
		base := strings.TrimSuffix(n, ".zip")
		if base != name && !strings.HasPrefix(base, name+"_") {
			continue
		}
		p := filepath.Join(dir, n)
		var info modInfo
		var err error
		if e.IsDir() {
			var b []byte
			if b, err = fs.ReadFile(os.DirFS(p), "info.json"); err == nil {
				info, err = parseInfo(b)
			}
		} else if strings.HasSuffix(n, ".zip") {
			info, err = zipInfo(p)
		} else {
			continue
		}
		if err != nil || info.Name != name {
			continue
		}
		if best == "" || versionLess(bestInfo.Version, info.Version) {
			best, bestInfo = p, info
		}
	}
	if best == "" {
		return "", modInfo{}, fmt.Errorf("not installed in %s", dir)
	}
	return best, bestInfo, nil
}

// versionLess compares dotted numeric versions.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// writeModList enables exactly the enabled mods (built-in ones not required are disabled).
func writeModList(mods string, enabled map[string]bool) error {
	type entry struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	list := []entry{}
	for _, b := range builtin {
		if b != "core" {
			list = append(list, entry{b, enabled[b]})
		}
	}
	for _, n := range slices.Sorted(func(yield func(string) bool) {
		for k := range enabled {
			if !slices.Contains(builtin, k) && !yield(k) {
				return
			}
		}
	}) {
		list = append(list, entry{n, true})
	}
	b, _ := json.MarshalIndent(map[string]any{"mods": list}, "", "  ")
	return os.WriteFile(filepath.Join(mods, "mod-list.json"), b, 0o600)
}
