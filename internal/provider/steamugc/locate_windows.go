//go:build windows

package steamugc

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

var (
	libraryRe = regexp.MustCompile(`"path"\s+"([^"]+)"`)
	ugcVerRe  = regexp.MustCompile(`SteamAPI_SteamUGC_v0(\d\d)\x00`)

	foundMu  sync.Mutex
	foundDLL string
)

// libraries are the Steam library folders (steamapps\common of each).
func libraries() []string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Valve\Steam`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	root, _, err := k.GetStringValue("SteamPath")
	_ = k.Close()
	if err != nil || root == "" {
		return nil
	}
	root = filepath.FromSlash(root)
	out := []string{filepath.Join(root, "steamapps", "common")}
	if b, err := os.ReadFile(filepath.Join(root, "steamapps", "libraryfolders.vdf")); err == nil { //nolint:gosec // G304: Steam's own file
		for _, m := range libraryRe.FindAllSubmatch(b, -1) {
			p := filepath.Join(filepath.FromSlash(string(bytes.ReplaceAll(m[1], []byte(`\\`), []byte(`\`)))), "steamapps", "common")
			if !filepathContains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

func filepathContains(list []string, p string) bool {
	for _, q := range list {
		if equalFold(q, p) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b) || (len(a) == len(b) && bytes.EqualFold([]byte(a), []byte(b)))
}

// dllScore: 0 = unusable; else the ISteamUGC accessor version, +1000 with
// SteamAPI_InitFlat (SDK 1.58+). Read from the export names in the file.
func dllScore(path string) int {
	b, err := os.ReadFile(path) //nolint:gosec // G304: a game's steam_api64.dll
	if err != nil {
		return 0
	}
	best := 0
	for _, m := range ugcVerRe.FindAllSubmatch(b, -1) {
		if v, _ := strconv.Atoi(string(m[1])); v > best {
			best = v
		}
	}
	if best < 14 || !bytes.Contains(b, []byte("SteamAPI_ISteamUGC_SetItemUpdateLanguage\x00")) {
		return 0
	}
	if bytes.Contains(b, []byte("SteamAPI_InitFlat\x00")) {
		best += 1000
	}
	return best
}

// findDLL picks the newest steam_api64.dll of the installed games.
func findDLL() (string, error) {
	foundMu.Lock()
	defer foundMu.Unlock()
	if foundDLL != "" {
		if _, err := os.Stat(foundDLL); err == nil {
			return foundDLL, nil
		}
	}
	best, bestScore := "", 0
	for _, lib := range libraries() {
		for _, pat := range []string{`*\steam_api64.dll`, `*\*\steam_api64.dll`, `*\*\*\steam_api64.dll`, `*\*\*\*\steam_api64.dll`} {
			ms, _ := filepath.Glob(filepath.Join(lib, pat))
			for _, m := range ms {
				if s := dllScore(m); s > bestScore {
					best, bestScore = m, s
				}
			}
		}
	}
	if best == "" {
		return "", ErrNoSteamAPI
	}
	foundDLL = best
	return best, nil
}
