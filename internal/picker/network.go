package picker

import "strings"

// IsNetworkPath reports whether p is on a network share: a UNC path
// (\\server\share, //server/share, \\?\UNC\...) or, on Windows, a drive
// letter mapped to a share. Such folders are never used as the dialog's start
// folder: resolving a slow or dead share blocks for seconds.
func IsNetworkPath(p string) bool {
	p = strings.TrimSpace(p)
	if len(p) < 2 {
		return false
	}
	if isSep(p[0]) && isSep(p[1]) {
		// \\?\C:\... and \\.\C:\... are local long-path / device forms; \\?\UNC\... is a share.
		if len(p) >= 4 && (p[2] == '?' || p[2] == '.') && isSep(p[3]) {
			rest := p[4:]
			if len(rest) >= 4 && strings.EqualFold(rest[:3], "UNC") && isSep(rest[3]) {
				return true
			}
			return isRemoteDrive(rest)
		}
		return true
	}
	return isRemoteDrive(p)
}

func isSep(c byte) bool { return c == '\\' || c == '/' }

// driveRoot returns "X:\" for a path starting with a drive letter, else "".
func driveRoot(p string) string {
	if len(p) < 2 || p[1] != ':' {
		return ""
	}
	c := p[0] | 0x20
	if c < 'a' || c > 'z' {
		return ""
	}
	return p[:2] + `\`
}
