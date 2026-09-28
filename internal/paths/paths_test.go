package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDataDirEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "nested", "data")
	t.Setenv(EnvDataDir, want)

	got, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Fatalf("data dir not created: %v", err)
	}
}

func TestDataDirNextToExe(t *testing.T) {
	t.Setenv(EnvDataDir, "")

	got, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if want := filepath.Join(filepath.Dir(exe), "data"); got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
}
