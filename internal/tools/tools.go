// Package tools describes the helpers the app provisions into <data>\tools
// (steamcmd, steam_api64.dll): everything the app runs or loads lives next
// to the portable binary, set up automatically or by one Settings button.
package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State of a provisioned helper.
type State string

// States: Missing (not set up yet), Working (downloading / copying), Ready,
// Failed (the last attempt failed; Error says why).
const (
	Missing State = "missing"
	Working State = "working"
	Ready   State = "ready"
	Failed  State = "error"
)

// Status is a helper's view for Settings › Платформы and GET /api/tools.
type Status struct {
	Name   string `json:"name"`
	State  State  `json:"state"`
	Path   string `json:"path,omitempty"`   // the file in <data>\tools when ready
	Source string `json:"source,omitempty"` // where it came from (download URL, the game folder it was copied from)
	Error  string `json:"error,omitempty"`
	At     string `json:"at,omitempty"` // provisioned at (RFC 3339)
}

// Dir is <data>\tools\<name>.
func Dir(dataDir, name string) string { return filepath.Join(dataDir, "tools", name) }

// Progress tracks one helper's running / failed provisioning (Working and
// Failed are not on disk).
type Progress struct {
	mu      sync.Mutex
	working bool
	err     string
}

// Begin marks a provisioning as running; false = one is already running.
func (p *Progress) Begin() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.working {
		return false
	}
	p.working, p.err = true, ""
	return true
}

// End records the outcome of Begin's provisioning.
func (p *Progress) End(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.working = false
	p.err = ""
	if err != nil {
		p.err = err.Error()
	}
}

// Apply sets st.State / st.Error from the progress over the on-disk state
// (ready = the file is there and valid).
func (p *Progress) Apply(st *Status, ready bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.working:
		st.State = Working
	case ready:
		st.State = Ready
	case p.err != "":
		st.State, st.Error = Failed, p.err
	default:
		st.State = Missing
	}
}

// ErrBusy: a provisioning of the helper is already running.
var ErrBusy = errors.New("tools: already being set up")

// SHA256 is the hex SHA-256 of a file.
func SHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file in the app's tools folder or the one it copies
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CopyFile copies src to dst through dst.tmp (atomic rename), creating
// dst's folder; it returns dst's SHA-256.
func CopyFile(src, dst string, limit int64) (string, error) {
	in, err := os.Open(src) //nolint:gosec // G304: the caller's source file
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700) //nolint:gosec // G302/G304: an executable or DLL in the app's tools folder
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = fmt.Errorf("%s: larger than %d bytes", src, limit)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Stamp formats t for Status.At ("" = zero).
func Stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
