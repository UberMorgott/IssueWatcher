package tools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProgressStates(t *testing.T) {
	var p Progress
	st := Status{}
	p.Apply(&st, false)
	if st.State != Missing {
		t.Fatalf("idle: %+v", st)
	}
	if !p.Begin() || p.Begin() {
		t.Fatal("Begin must run once at a time")
	}
	p.Apply(&st, true)
	if st.State != Working {
		t.Fatalf("working: %+v", st)
	}
	p.End(errors.New("HTTP 503"))
	st = Status{}
	p.Apply(&st, false)
	if st.State != Failed || st.Error != "HTTP 503" {
		t.Fatalf("failed: %+v", st)
	}
	st = Status{}
	p.Apply(&st, true)
	if st.State != Ready || st.Error != "" {
		t.Fatalf("ready wins over an old error: %+v", st)
	}
}

func TestCopyFileHashAndLimit(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.dll")
	if err := os.WriteFile(src, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "tools", "x", "out.dll")
	sum, err := CopyFile(src, dst, 1<<10)
	if err != nil || sum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("copy: %s %v", sum, err)
	}
	if got, err := SHA256(dst); err != nil || got != sum {
		t.Fatalf("hash: %s %v", got, err)
	}
	if _, err := CopyFile(src, filepath.Join(dir, "big.dll"), 2); err == nil {
		t.Fatal("over the limit must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "big.dll.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp left: %v", err)
	}
}
