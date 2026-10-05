//go:build windows

package smoke

import (
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// steamPath is the Steam install (HKCU\Software\Valve\Steam\SteamPath), "" = none.
func steamPath() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Valve\Steam`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue("SteamPath")
	if err != nil || v == "" {
		return ""
	}
	return filepath.Clean(filepath.FromSlash(v))
}
