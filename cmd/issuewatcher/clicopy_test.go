package main

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fakePE is a minimal PE32+ header with the given subsystem.
func fakePE(subsystem uint16) []byte {
	b := make([]byte, 0x200)
	b[0], b[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(b[0x3c:], 0x80)
	copy(b[0x80:], "PE\x00\x00")
	opt := 0x80 + 4 + 20
	binary.LittleEndian.PutUint16(b[opt:], 0x20b)
	binary.LittleEndian.PutUint16(b[opt+68:], subsystem)
	return b
}

func TestConsoleCopy(t *testing.T) {
	gui := fakePE(subsystemGUI)
	out, ok, err := consoleCopy(gui)
	if err != nil || !ok {
		t.Fatalf("gui: ok %v err %v", ok, err)
	}
	off, _ := subsystemOffset(out)
	if got := binary.LittleEndian.Uint16(out[off:]); got != subsystemConsole {
		t.Fatalf("subsystem %d", got)
	}
	if binary.LittleEndian.Uint16(gui[off:]) != subsystemGUI {
		t.Fatal("input modified")
	}
	gui[off] = 0 // everything else identical
	out[off] = 0
	if string(gui) != string(out) {
		t.Fatal("copy differs beyond the subsystem field")
	}
	if _, ok, err := consoleCopy(fakePE(subsystemConsole)); ok || err != nil {
		t.Fatalf("console build: ok %v err %v", ok, err)
	}
	if _, _, err := consoleCopy([]byte("MZ not a pe")); err == nil {
		t.Fatal("garbage accepted")
	}
	// This test binary is a real (console) PE.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(self) //nolint:gosec // G304: the test binary itself
	if _, ok, err := consoleCopy(b); ok || err != nil {
		t.Fatalf("test binary: ok %v err %v", ok, err)
	}
}

// TestHelperSleep is the running process of TestWriteCLIReplacesRunningCopy.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("IW_TEST_HELPER_SLEEP") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(time.Minute)
}

// A copy that is running (an MCP server, a shell call) is moved aside, not
// left stale; the moved one is removed on a later refresh.
func TestWriteCLIReplacesRunningCopy(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, cliExeName)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self) //nolint:gosec // G304: the test binary itself
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCLI(dst, b); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(dst, "-test.run=^TestHelperSleep$") //nolint:gosec,noctx // our test binary
	cmd.Env = append(os.Environ(), "IW_TEST_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	time.Sleep(300 * time.Millisecond)
	if err := writeCLI(dst, []byte("new version")); err != nil {
		t.Fatalf("replace running copy: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new version" { //nolint:gosec // G304: test temp dir
		t.Fatalf("dst %q", got)
	}
	old := filepath.Join(dir, "."+cliExeName+".old")
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("running copy not parked aside: %v", err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := writeCLI(dst, []byte("new version")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal(".old leftover not removed once the old copy exited")
	}
	if _, err := os.Stat(filepath.Join(dir, "."+cliExeName+".new")); err == nil {
		t.Fatal(".new leftover")
	}
}
