// Command releasekey manages the Ed25519 key that signs IssueWatcher release
// manifests (release.ps1). The private key lives outside the repository,
// readable only by the current user; its public half is compiled into the app
// (internal/selfupdate/pubkey.go). Nothing here prints the private key.
//
//	releasekey init     [-key path]   create the key if missing; print the public key
//	releasekey pub      [-key path]   print the public key
//	releasekey manifest [-key path] -exe file -version vX.Y.Z [-min vX.Y.Z] -out dir
//	                                  write <asset>.sha256, manifest.json, manifest.json.sig
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/secret"
	"github.com/UberMorgott/issuewatcher/internal/selfupdate"
)

type keyFile struct {
	Seed      string    `json:"seed"` // base64 Ed25519 seed (private)
	PublicKey string    `json:"publicKey"`
	CreatedAt time.Time `json:"createdAt"`
}

func defaultKeyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "issuewatcher", "release-ed25519.key")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "releasekey:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: releasekey init|pub|manifest [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	keyPath := fs.String("key", defaultKeyPath(), "private key file")
	exe := fs.String("exe", "", "release executable (manifest)")
	version := fs.String("version", "", "release tag vX.Y.Z (manifest)")
	minVersion := fs.String("min", "", "minimum version that may update to this one (manifest)")
	out := fs.String("out", "", "output directory (manifest)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "init":
		pub, created, err := initKey(*keyPath)
		if err != nil {
			return err
		}
		if created {
			fmt.Fprintln(os.Stderr, "created", *keyPath)
		}
		fmt.Println(pub)
		return nil
	case "pub":
		priv, err := load(*keyPath)
		if err != nil {
			return err
		}
		fmt.Println(pubString(priv))
		return nil
	case "manifest":
		if *exe == "" || *version == "" || *out == "" {
			return errors.New("manifest needs -exe, -version and -out")
		}
		priv, err := load(*keyPath)
		if err != nil {
			return err
		}
		return writeManifest(priv, *exe, *version, *minVersion, *out)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func pubString(priv ed25519.PrivateKey) string {
	pub, _ := priv.Public().(ed25519.PublicKey)
	return base64.StdEncoding.EncodeToString(pub)
}

// initKey creates the key file if it does not exist (never overwrites one).
func initKey(path string) (string, bool, error) {
	if priv, err := load(path); err == nil {
		return pubString(priv), false, nil
	} else if !errors.Is(err, secret.ErrNotFound) {
		return "", false, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", false, err
	}
	k := keyFile{Seed: base64.StdEncoding.EncodeToString(priv.Seed()), PublicKey: pubString(priv), CreatedAt: time.Now().UTC()}
	if err := secret.WriteJSON(path, k); err != nil { // owner-only DACL on the file and its folder
		return "", false, err
	}
	return k.PublicKey, true, nil
}

func load(path string) (ed25519.PrivateKey, error) {
	var k keyFile
	if err := secret.ReadJSON(path, &k); err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(k.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: bad seed", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// writeManifest signs the manifest of exe. It refuses a key whose public half
// is not the one compiled into the app: such a release could never install.
func writeManifest(priv ed25519.PrivateKey, exe, version, minVersion, out string) error {
	embedded, err := selfupdate.PublicKey()
	if err != nil {
		return err
	}
	if !embedded.Equal(priv.Public()) {
		return errors.New("this key is not the release key compiled into the app (internal/selfupdate/pubkey.go)")
	}
	f, err := os.Open(exe) //nolint:gosec // G304: the release build output named by the caller
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	m := selfupdate.Manifest{
		Version: version, Asset: selfupdate.AssetName("windows", "amd64"),
		Size: size, SHA256: hex.EncodeToString(h.Sum(nil)), MinVersion: minVersion,
	}
	if err := m.Validate(); err != nil {
		return err
	}
	body, err := m.Encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}
	files := map[string][]byte{
		selfupdate.ManifestAsset:  body,
		selfupdate.SignatureAsset: selfupdate.Sign(priv, body),
		m.Asset + ".sha256":       []byte(m.SHA256 + "  " + m.Asset + "\n"),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil {
			return err
		}
	}
	if _, err := selfupdate.VerifyManifest(embedded, body, files[selfupdate.SignatureAsset]); err != nil {
		return fmt.Errorf("self-check: %w", err)
	}
	fmt.Printf("manifest %s %s size=%d sha256=%s\n", m.Version, m.Asset, m.Size, m.SHA256)
	return nil
}
