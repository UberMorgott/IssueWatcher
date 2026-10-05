//go:build !windows

package steamugc

import "errors"

// NewAPI is Windows only.
func NewAPI(string) (API, error) { return nil, errors.New("steamugc: Windows only") }
