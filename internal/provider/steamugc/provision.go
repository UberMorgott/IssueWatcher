package steamugc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/UberMorgott/issuewatcher/internal/secret"
	"github.com/UberMorgott/issuewatcher/internal/tools"
)

// steam_api64.dll is the Steamworks SDK redistributable: Valve hands it out
// only with the SDK (partner sign-in) and every Steam game ships its own
// copy. The app copies one, once, from the owner's installed games into
// data\tools\steamworks (SHA-256 recorded and checked before each use) and
// loads it only from there; nothing is redistributed and no game folder is
// written. The Steam client itself (steamclient64.dll) is what the DLL talks to.

const (
	dllName    = "steam_api64.dll"
	recordName = "steam_api64.json"
	maxDLL     = 64 << 20
)

// record is tools\steamworks\steam_api64.json.
type record struct {
	SHA256 string `json:"sha256"`
	Source string `json:"source"` // the game file it was copied from
	At     string `json:"at"`
}

func (c *Client) toolDir() string { return tools.Dir(c.opts.DataDir, "steamworks") }

// DLLPath is the app's own steam_api64.dll.
func (c *Client) DLLPath() string { return filepath.Join(c.toolDir(), dllName) }

// provisioned is the record of a DLL whose file still matches it.
func (c *Client) provisioned() (record, bool) {
	var r record
	if err := secret.ReadJSON(filepath.Join(c.toolDir(), recordName), &r); err != nil || r.SHA256 == "" {
		return r, false
	}
	sum, err := tools.SHA256(c.DLLPath())
	return r, err == nil && sum == r.SHA256
}

// Tool is steam_api64.dll's provisioning status.
func (c *Client) Tool() tools.Status {
	st := tools.Status{Name: "steamworks"}
	r, ok := c.provisioned()
	if ok {
		st.Path, st.Source, st.At = c.DLLPath(), r.Source, r.At
	}
	c.prog.Apply(&st, ok)
	return st
}

// Provision copies the newest usable steam_api64.dll of the installed games
// into data\tools\steamworks unless a valid copy is there already
// (automatic at startup; Settings › Платформы retries it).
func (c *Client) Provision(context.Context) (tools.Status, error) {
	if !c.prog.Begin() {
		return c.Tool(), tools.ErrBusy
	}
	_, err := c.provision()
	c.prog.End(err)
	return c.Tool(), err
}

func (c *Client) provision() (string, error) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	if _, ok := c.provisioned(); ok {
		return c.DLLPath(), nil
	}
	src, err := c.opts.Find()
	if err != nil {
		return "", err
	}
	sum, err := tools.CopyFile(src, c.DLLPath(), maxDLL)
	if err != nil {
		return "", fmt.Errorf("steam: copy %s: %w", src, err)
	}
	r := record{SHA256: sum, Source: src, At: tools.Stamp(c.opts.Now())}
	if err := secret.WriteJSON(filepath.Join(c.toolDir(), recordName), r); err != nil {
		_ = os.Remove(c.DLLPath())
		return "", err
	}
	return c.DLLPath(), nil
}

// steamAPI is the app's steam_api64.dll, provisioned on first use.
func (c *Client) steamAPI() (string, error) {
	if _, ok := c.provisioned(); ok {
		return c.DLLPath(), nil
	}
	if !c.prog.Begin() {
		return "", tools.ErrBusy
	}
	p, err := c.provision()
	c.prog.End(err)
	return p, err
}

// canProvision: ready, or a DLL to copy exists (dry runs: nothing written).
func (c *Client) canProvision() error {
	if _, ok := c.provisioned(); ok {
		return nil
	}
	_, err := c.opts.Find()
	return err
}
