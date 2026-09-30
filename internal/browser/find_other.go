//go:build !windows

package browser

// Find is Windows-only for now (the app ships for Windows).
func Find() (Exe, error) { return Exe{}, ErrNoBrowser }
