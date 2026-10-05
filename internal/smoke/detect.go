package smoke

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Install is a Factorio installation.
type Install struct {
	Exe  string `json:"exe"`  // bin/x64/factorio.exe
	Data string `json:"data"` // read-data (the game's data directory)
	Mods string `json:"mods"` // the owner's mods folder (dependencies are copied from it; never written)
}

// exeRel is factorio.exe under an install root.
var exeRel = filepath.Join("bin", "x64", "factorio.exe")

// install resolves the profile's override (install root or factorio.exe) or detects one.
func (r Runner) install(override string) (Install, error) {
	if override != "" {
		exe := override
		if st, err := os.Stat(exe); err == nil && st.IsDir() {
			exe = filepath.Join(exe, exeRel)
		}
		if st, err := os.Stat(exe); err != nil || st.IsDir() {
			return Install{}, fmt.Errorf("%w: no factorio executable at %s", ErrUnavailable, override)
		}
		return InstallAt(exe), nil
	}
	detect := r.Detect
	if detect == nil {
		detect = DetectFactorio
	}
	return detect()
}

// DetectFactorio finds the owner's Factorio: every Steam library
// (steamapps/common/Factorio), then the standalone install locations.
func DetectFactorio() (Install, error) {
	var roots []string
	for _, lib := range steamLibraries() {
		roots = append(roots, filepath.Join(lib, "steamapps", "common", "Factorio"))
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if p := os.Getenv(env); p != "" {
			roots = append(roots, filepath.Join(p, "Factorio"), filepath.Join(p, "Steam", "steamapps", "common", "Factorio"))
		}
	}
	for _, root := range roots {
		exe := filepath.Join(root, exeRel)
		if st, err := os.Stat(exe); err == nil && !st.IsDir() {
			return InstallAt(exe), nil
		}
	}
	return Install{}, fmt.Errorf("%w: no Factorio install found (Steam libraries, Program Files); set the install path in the publish profile", ErrUnavailable)
}

// InstallAt describes the install of exe: data next to it, mods from its
// config (config-path.cfg → config.ini → path.write-data), else %APPDATA%\Factorio\mods.
func InstallAt(exe string) Install {
	bin := filepath.Dir(exe)
	root := filepath.Clean(filepath.Join(bin, "..", ".."))
	in := Install{Exe: exe, Data: filepath.Join(root, "data")}
	system := ""
	if a := os.Getenv("APPDATA"); a != "" {
		system = filepath.Join(a, "Factorio")
	}
	expand := func(p string) string {
		p = strings.ReplaceAll(p, "__PATH__executable__", filepath.ToSlash(bin))
		p = strings.ReplaceAll(p, "__PATH__system-write-data__", filepath.ToSlash(system))
		p = strings.ReplaceAll(p, "__PATH__system-read-data__", filepath.ToSlash(filepath.Join(root, "data")))
		return filepath.Clean(filepath.FromSlash(p))
	}
	cfgPath := filepath.Join(root, "config", "config.ini")
	if v := iniValue(filepath.Join(root, "config-path.cfg"), "", "config-path"); v != "" {
		cfgPath = filepath.Join(expand(v), "config.ini")
		if strings.HasSuffix(strings.ToLower(v), ".ini") {
			cfgPath = expand(v)
		}
	}
	if v := iniValue(cfgPath, "path", "read-data"); v != "" {
		in.Data = expand(v)
	}
	switch v := iniValue(cfgPath, "path", "write-data"); {
	case v != "":
		in.Mods = filepath.Join(expand(v), "mods")
	case system != "":
		in.Mods = filepath.Join(system, "mods")
	}
	return in
}

// iniValue reads key of section ("" = before any section) from an ini file.
func iniValue(path, section, key string) string {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the game's own config
	if err != nil {
		return ""
	}
	cur := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";"):
		case strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]"):
			cur = l[1 : len(l)-1]
		case cur == section:
			if k, v, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == key {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

var vdfPath = regexp.MustCompile(`"path"\s+"((?:[^"\\]|\\.)*)"`)

// steamLibraries lists Steam library roots (the Steam install first).
func steamLibraries() []string {
	steam := steamPath()
	if steam == "" {
		return nil
	}
	libs := []string{steam}
	b, err := os.ReadFile(filepath.Join(steam, "steamapps", "libraryfolders.vdf")) //nolint:gosec // G304: Steam's own file
	if err != nil {
		return libs
	}
	for _, m := range vdfPath.FindAllSubmatch(b, -1) {
		p := filepath.Clean(strings.ReplaceAll(string(m[1]), `\\`, `\`))
		if !strings.EqualFold(p, filepath.Clean(steam)) {
			libs = append(libs, p)
		}
	}
	return libs
}
