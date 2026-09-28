//go:build windows

package instance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// Lock is held for the process lifetime; Windows frees the mutex if the
// process dies, so a crash never leaves a stale lock behind.
type Lock struct{ h windows.Handle }

// Acquire takes the single-instance mutex for dataDir. Portable copies in
// different folders are independent instances. Returns ErrAlreadyRunning when
// another process holds it.
func Acquire(dataDir string) (*Lock, error) {
	sum := sha256.Sum256([]byte(strings.ToLower(dataDir)))
	name, err := windows.UTF16PtrFromString(`Local\IssueWatcher-` + hex.EncodeToString(sum[:8]))
	if err != nil {
		return nil, fmt.Errorf("instance: mutex name: %w", err)
	}
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, fmt.Errorf("instance: create mutex: %w", err)
	}
	return &Lock{h: h}, nil
}

// Release frees the mutex.
func (l *Lock) Release() {
	if l != nil && l.h != 0 {
		_ = windows.CloseHandle(l.h)
		l.h = 0
	}
}
