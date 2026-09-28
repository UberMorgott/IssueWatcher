//go:build !windows

package secret

import (
	"fmt"
	"os"
)

// restrict limits path to the owner.
func restrict(path string, dir bool) error {
	mode := os.FileMode(0o600)
	if dir {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("secret: chmod: %w", err)
	}
	return nil
}
