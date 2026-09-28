package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// Release assets. Every release carries the executable, its SHA-256 line, the
// manifest and the manifest's detached Ed25519 signature (base64 of the
// signature over the exact bytes of manifest.json).
const (
	ManifestAsset  = "manifest.json"
	SignatureAsset = "manifest.json.sig"
)

// AssetName is the release executable for goos/goarch.
func AssetName(goos, goarch string) string {
	name := "issuewatcher-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// ThisAsset is the release executable for the running platform.
func ThisAsset() string { return AssetName(runtime.GOOS, runtime.GOARCH) }

// Manifest describes a release executable. The release pipeline
// (release.ps1 → cmd/releasekey) signs it; the updater trusts nothing in a
// release that the signature does not cover.
type Manifest struct {
	Version string `json:"version"` // the release tag, "vX.Y.Z[-pre]"
	Asset   string `json:"asset"`   // AssetName
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"` // lowercase hex
	// MinVersion: installs older than this must first update to an
	// intermediate release (none so far).
	MinVersion string `json:"minVersion,omitempty"`
}

// Encode is the canonical manifest.json body the signature covers.
func (m Manifest) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Validate checks the fields' shape.
func (m Manifest) Validate() error {
	switch {
	case !IsRelease(m.Version):
		return fmt.Errorf("manifest: bad version %q", m.Version)
	case m.Asset == "" || strings.ContainsAny(m.Asset, `/\`):
		return fmt.Errorf("manifest: bad asset %q", m.Asset)
	case m.Size <= 0 || m.Size > maxAssetSize:
		return fmt.Errorf("manifest: bad size %d", m.Size)
	case m.MinVersion != "" && !IsRelease(m.MinVersion):
		return fmt.Errorf("manifest: bad minVersion %q", m.MinVersion)
	}
	if b, err := hex.DecodeString(m.SHA256); err != nil || len(b) != 32 || m.SHA256 != strings.ToLower(m.SHA256) {
		return fmt.Errorf("manifest: bad sha256 %q", m.SHA256)
	}
	return nil
}

// maxAssetSize bounds a download: far above a ~10 MB build.
const maxAssetSize = 128 << 20

// ErrSignature is a manifest whose signature does not verify with the
// embedded public key.
var ErrSignature = errors.New("manifest signature is not valid for this app's release key")

// Sign returns the signature file body for manifest bytes (release pipeline).
func Sign(priv ed25519.PrivateKey, manifest []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, manifest)) + "\n")
}

// VerifyManifest checks sig (a SignatureAsset body) over body with pub and
// decodes the manifest. Nothing in an unsigned or badly signed manifest is
// read.
func VerifyManifest(pub ed25519.PublicKey, body, sig []byte) (Manifest, error) {
	var m Manifest
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize || len(pub) != ed25519.PublicKeySize {
		return m, ErrSignature
	}
	if !ed25519.Verify(pub, body, raw) {
		return m, ErrSignature
	}
	if err := json.Unmarshal(body, &m); err != nil { // unknown (newer) fields are fine: signed anyway
		return m, fmt.Errorf("manifest: %w", err)
	}
	return m, m.Validate()
}

// PublicKey is the release signing key's public half compiled into this build.
func PublicKey() (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("selfupdate: no valid release public key in this build")
	}
	return ed25519.PublicKey(b), nil
}
