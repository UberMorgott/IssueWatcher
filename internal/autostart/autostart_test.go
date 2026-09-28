package autostart

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// testEntry lives under HKCU\Software\IssueWatcherTest and is removed afterwards.
func testEntry(t *testing.T) Entry {
	t.Helper()
	const key = `Software\IssueWatcherTest`
	t.Cleanup(func() {
		if err := registry.DeleteKey(registry.CURRENT_USER, key); err != nil && !errors.Is(err, registry.ErrNotExist) {
			t.Errorf("cleanup: %v", err)
		}
	})
	return Entry{Key: key, Name: "IssueWatcher"}
}

func TestEntryLifecycle(t *testing.T) {
	e := testEntry(t)
	exe := `C:\Portable\IssueWatcher\issuewatcher.exe`
	if on, err := e.Enabled(exe); err != nil || on {
		t.Fatalf("fresh: %v %v", on, err)
	}
	if err := e.Set(true, exe); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.Get(); v != `"C:\Portable\IssueWatcher\issuewatcher.exe" --minimized` {
		t.Fatalf("value %q", v)
	}
	if on, _ := e.Enabled(`c:\portable\issuewatcher\ISSUEWATCHER.exe`); !on {
		t.Fatal("path compare must ignore case")
	}

	// Folder moved: the old value no longer counts as enabled; Refresh rewrites it.
	moved := `D:\Tools\IW\issuewatcher.exe`
	if on, _ := e.Enabled(moved); on {
		t.Fatal("stale path reported as enabled")
	}
	if wrote, err := e.Refresh(true, moved); err != nil || !wrote {
		t.Fatalf("refresh: %v %v", wrote, err)
	}
	if on, _ := e.Enabled(moved); !on {
		t.Fatal("not enabled after refresh")
	}
	if wrote, _ := e.Refresh(true, moved); wrote {
		t.Fatal("refresh rewrote an up-to-date value")
	}
	if wrote, _ := e.Refresh(false, `X:\other.exe`); wrote {
		t.Fatal("refresh wrote while the setting is off")
	}

	if err := e.Set(false, moved); err != nil {
		t.Fatal(err)
	}
	if v, err := e.Get(); err != nil || v != "" {
		t.Fatalf("after delete: %q %v", v, err)
	}
	if err := e.Set(false, moved); err != nil {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestExePath(t *testing.T) {
	for in, want := range map[string]string{
		`"C:\a b\x.exe" --minimized`: `C:\a b\x.exe`,
		`C:\x.exe --minimized`:       `C:\x.exe`,
		`C:\x.exe`:                   `C:\x.exe`,
		`"broken`:                    "",
		"":                           "",
	} {
		if got := ExePath(in); got != want {
			t.Errorf("ExePath(%q) = %q, want %q", in, got, want)
		}
	}
}
