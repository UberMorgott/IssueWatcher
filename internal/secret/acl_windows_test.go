package secret

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWrittenFileHasProtectedOwnerOnlyDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s", "k.json")
	if err := WriteJSON(path, sample{A: "x"}); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("DACL not protected: inherited entries still apply")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl.AceCount != 1 {
		t.Fatalf("ACE count = %d, want 1 (current user only)", dacl.AceCount)
	}
}
