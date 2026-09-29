package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestProtectedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "p.json")
	var got sample
	if err := ReadProtectedJSON(path, &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: err %v, want ErrNotFound", err)
	}
	want := sample{A: "cookie-value-xyz", B: 7}
	if err := WriteProtectedJSON(path, want); err != nil {
		t.Fatal(err)
	}
	if err := ReadProtectedJSON(path, &got); err != nil || got != want {
		t.Fatalf("read back %+v err %v", got, err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test temp file
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && bytes.Contains(raw, []byte("cookie-value-xyz")) {
		t.Fatal("plaintext secret on disk")
	}
	// A plain (unprotected) file is not accepted as protected.
	if err := WriteJSON(path, protectedFile{Protected: []byte("garbage")}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if err := ReadProtectedJSON(path, &got); err == nil {
			t.Fatal("garbage decrypted")
		}
	}
}
