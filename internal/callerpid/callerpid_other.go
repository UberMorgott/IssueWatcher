//go:build !windows

package callerpid

import "errors"

// LoopbackPID is only implemented on Windows.
func LoopbackPID(remoteAddr, localAddr string) (uint32, error) {
	if _, _, err := endpoints(remoteAddr, localAddr); err != nil {
		return 0, err
	}
	return 0, errors.New("callerpid: not supported on this OS")
}
