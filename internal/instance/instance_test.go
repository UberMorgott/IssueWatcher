package instance

import (
	"errors"
	"testing"
	"time"
)

func TestRuntimeRoundTripAndRemove(t *testing.T) {
	dir := t.TempDir()
	want := Runtime{PID: 42, Port: 51234, URL: "http://127.0.0.1:51234", Token: "tok", Version: "dev",
		StartedAt: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	if err := WriteRuntime(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRuntime(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ReadRuntime = %+v, want %+v", got, want)
	}

	// Another pid must not delete our file.
	if err := RemoveRuntime(dir, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRuntime(dir); err != nil {
		t.Fatalf("runtime removed by foreign pid: %v", err)
	}
	if err := RemoveRuntime(dir, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRuntime(dir); err == nil {
		t.Fatal("runtime still present after RemoveRuntime")
	}
}

func TestAcquireIsExclusivePerDataDir(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Acquire err = %v, want ErrAlreadyRunning", err)
	}
	other, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatalf("different data dir must not conflict: %v", err)
	}
	other.Release()

	first.Release()
	again, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	again.Release()
}
