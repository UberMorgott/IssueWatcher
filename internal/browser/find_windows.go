package browser

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// chromium lists the Chromium-based browsers by exe name (lower case).
var chromium = map[string]string{
	"chrome.exe": "Chrome", "msedge.exe": "Edge", "brave.exe": "Brave", "vivaldi.exe": "Vivaldi",
	"opera.exe": "Opera", "browser.exe": "Yandex", "chromium.exe": "Chromium",
}

// Find returns the browser to drive: the default browser when it is
// Chromium-based, else Edge / Chrome.
func Find() (Exe, error) {
	if e, ok := defaultBrowser(); ok {
		return e, nil
	}
	for _, name := range []string{"msedge.exe", "chrome.exe"} {
		if p := appPath(name); p != "" {
			return Exe{Path: p, Name: chromium[name]}, nil
		}
	}
	for _, p := range knownPaths() {
		if fileExists(p) {
			return Exe{Path: p, Name: chromium[strings.ToLower(filepath.Base(p))]}, nil
		}
	}
	return Exe{}, ErrNoBrowser
}

// defaultBrowser reads the https handler's ProgId → its open command → exe.
func defaultBrowser() (Exe, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice`, registry.QUERY_VALUE)
	if err != nil {
		return Exe{}, false
	}
	prog, _, err := k.GetStringValue("ProgId")
	_ = k.Close()
	if err != nil || prog == "" {
		return Exe{}, false
	}
	c, err := registry.OpenKey(registry.CLASSES_ROOT, prog+`\shell\open\command`, registry.QUERY_VALUE)
	if err != nil {
		return Exe{}, false
	}
	cmd, _, err := c.GetStringValue("")
	_ = c.Close()
	if err != nil {
		return Exe{}, false
	}
	exe := commandExe(cmd)
	if exe == "" || !fileExists(exe) {
		return Exe{}, false
	}
	base := strings.ToLower(filepath.Base(exe))
	name, ok := chromium[base]
	switch {
	case strings.HasPrefix(strings.ToLower(prog), "centhtm"):
		name, ok = "Cent Browser", true
	case !ok && base == "chrome.exe": // other Chrome forks keep chrome.exe
		name, ok = "Chromium", true
	}
	if !ok {
		return Exe{}, false // Firefox & co: no CDP pipe
	}
	return Exe{Path: exe, Name: name}, true
}

// commandExe is the program of a shell open command (quoted or not).
func commandExe(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if end := strings.Index(cmd[1:], `"`); end >= 0 {
			return cmd[1 : end+1]
		}
		return ""
	}
	if i := strings.Index(strings.ToLower(cmd), ".exe"); i >= 0 {
		return cmd[:i+4]
	}
	return ""
}

func appPath(name string) string {
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		k, err := registry.OpenKey(root, `Software\Microsoft\Windows\CurrentVersion\App Paths\`+name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		p, _, err := k.GetStringValue("")
		_ = k.Close()
		if p = strings.Trim(p, `"`); err == nil && fileExists(p) {
			return p
		}
	}
	return ""
}

func knownPaths() []string {
	var out []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		out = append(out, filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
	}
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
