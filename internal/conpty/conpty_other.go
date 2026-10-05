//go:build !windows

package conpty

// Start is Windows only.
func Start(string, []string, Options) (Proc, error) { return nil, ErrUnsupported }
