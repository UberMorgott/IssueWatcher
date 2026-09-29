package picker

import "golang.org/x/sys/windows"

const driveRemote = 4 // DRIVE_REMOTE

// driveType is GetDriveTypeW (a variable so tests can fake mapped drives).
var driveType = func(root string) uint32 {
	r, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0
	}
	return windows.GetDriveType(r)
}

// isRemoteDrive reports whether p starts with a drive letter mapped to a network share.
func isRemoteDrive(p string) bool {
	root := driveRoot(p)
	return root != "" && driveType(root) == driveRemote
}
