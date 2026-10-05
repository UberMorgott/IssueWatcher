//go:build !windows

package smoke

// steamPath: Steam detection is Windows-only.
func steamPath() string { return "" }
