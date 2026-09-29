package secret

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protect encrypts b for the current user (DPAPI, no UI).
func protect(b []byte) ([]byte, error) {
	return dpapi(b, true)
}

// unprotect decrypts what protect produced for the same user.
func unprotect(b []byte) ([]byte, error) {
	return dpapi(b, false)
}

func dpapi(b []byte, encrypt bool) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("empty data")
	}
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]} //nolint:gosec // G115: secrets are tiny
	var out windows.DataBlob
	var err error
	if encrypt {
		err = windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	} else {
		err = windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) }() //nolint:gosec // G103: DPAPI-owned buffer
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil //nolint:gosec // G103: copy out of the DPAPI buffer
}
