package secret

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// protectedFile is the on-disk shape of WriteProtectedJSON: the JSON value
// encrypted for the current user (DPAPI on Windows), inside a file that
// WriteJSON already restricts to that user.
type protectedFile struct {
	Protected []byte `json:"protected"` // base64 in the file
}

// WriteProtectedJSON is WriteJSON with the value encrypted for the current
// user (Windows DPAPI CryptProtectData), for session cookies and API keys:
// a copied file is useless to another account or machine.
func WriteProtectedJSON(path string, v any) error {
	plain, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("secret: encode %s: %w", filepath.Base(path), err)
	}
	enc, err := protect(plain)
	if err != nil {
		return fmt.Errorf("secret: protect %s: %w", filepath.Base(path), err)
	}
	return WriteJSON(path, protectedFile{Protected: enc})
}

// ReadProtectedJSON decodes a file written by WriteProtectedJSON into v;
// ErrNotFound when missing.
func ReadProtectedJSON(path string, v any) error {
	var f protectedFile
	if err := ReadJSON(path, &f); err != nil {
		return err
	}
	plain, err := unprotect(f.Protected)
	if err != nil {
		return fmt.Errorf("secret: unprotect %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(plain, v); err != nil {
		return fmt.Errorf("secret: decode %s: %w", filepath.Base(path), err)
	}
	return nil
}
