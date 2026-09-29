//go:build !windows

package secret

// protect has no OS keystore off Windows: the file mode (0600) is the only
// protection there (the app ships for Windows; this keeps tests building).
func protect(b []byte) ([]byte, error) { return append([]byte(nil), b...), nil }

func unprotect(b []byte) ([]byte, error) { return append([]byte(nil), b...), nil }
