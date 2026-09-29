package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// cliExeName is the console-subsystem copy of the app kept next to it for
// interactive shells (docs/ARCHITECTURE.md → Control): cmd and PowerShell do
// not wait for a windowed (-H windowsgui) exe, so its output lands after the
// prompt and $LASTEXITCODE / %ERRORLEVEL% are lost. The copy is the same
// program with the PE subsystem field set to console: no second build, release
// asset or manifest entry, and every update refreshes it on the next start.
const cliExeName = "issuewatcher-cli.exe"

// PE subsystem values (IMAGE_OPTIONAL_HEADER.Subsystem).
const (
	subsystemGUI     = 2
	subsystemConsole = 3
)

// subsystemOffset is the file offset of the PE optional header's Subsystem
// field (at +68 in both PE32 and PE32+).
func subsystemOffset(b []byte) (int, error) {
	if len(b) < 0x40 || b[0] != 'M' || b[1] != 'Z' {
		return 0, errors.New("not a PE file")
	}
	pe := int(binary.LittleEndian.Uint32(b[0x3c:]))
	opt := pe + 4 + 20 // "PE\0\0" + COFF file header
	if pe <= 0 || opt+70 > len(b) || string(b[pe:pe+4]) != "PE\x00\x00" {
		return 0, errors.New("bad PE header")
	}
	if magic := binary.LittleEndian.Uint16(b[opt:]); magic != 0x10b && magic != 0x20b {
		return 0, fmt.Errorf("unknown PE optional header magic %#x", magic)
	}
	return opt + 68, nil
}

// consoleCopy returns exe with its GUI subsystem switched to console; ok is
// false for an exe that is not a windowed build (dev and test builds are
// console programs already: no copy needed).
func consoleCopy(exe []byte) (out []byte, ok bool, err error) {
	off, err := subsystemOffset(exe)
	if err != nil {
		return nil, false, err
	}
	if binary.LittleEndian.Uint16(exe[off:]) != subsystemGUI {
		return nil, false, nil
	}
	out = bytes.Clone(exe)
	binary.LittleEndian.PutUint16(out[off:], subsystemConsole)
	return out, true, nil
}

// isCLICopy reports whether this process runs from the console copy.
func isCLICopy(exe string) bool { return strings.EqualFold(filepath.Base(exe), cliExeName) }

// refreshCLI keeps cliExeName next to exe as its console copy (best effort: a
// read-only folder only costs the interactive-shell convenience).
func refreshCLI(log *slog.Logger, exe string) {
	b, err := os.ReadFile(exe) //nolint:gosec // G304: our own executable
	if err == nil {
		var ok bool
		if b, ok, err = consoleCopy(b); err == nil && ok {
			err = writeCLI(filepath.Join(filepath.Dir(exe), cliExeName), b)
		}
	}
	if err != nil {
		log.Warn("console copy for shells", "err", err)
	}
}

// writeCLI puts b at dst unless dst already holds it. A dst that is running
// (a shell call, an MCP server) cannot be overwritten but can be renamed:
// it moves aside to .<name>.old, removed on a later start once it has exited.
func writeCLI(dst string, b []byte) error {
	dir, base := filepath.Split(dst)
	old := filepath.Join(dir, "."+base+".old")
	_ = os.Remove(old) // leftover of an earlier refresh
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, b) { //nolint:gosec // G304: next to our own exe
		return nil
	}
	tmp := filepath.Join(dir, "."+base+".new")
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err == nil {
		return nil
	}
	if err := os.Rename(dst, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(tmp)
		return fmt.Errorf("move the running %s aside: %w", base, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(old) // fails while the old copy still runs
	return nil
}
