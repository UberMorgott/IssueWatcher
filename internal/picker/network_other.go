//go:build !windows

package picker

func isRemoteDrive(string) bool { return false }
