package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.0", 0},
		{"v0.1.1", "v0.1.0", 1},
		{"0.2.0", "v0.10.0", -1},
		{"v1.0.0", "v1.0.0-rc.1", 1},
		{"v1.0.0-rc.2", "v1.0.0-rc.10", -1},
		{"v1.0.0-alpha", "v1.0.0-alpha.1", -1},
		{"v1.0.0-alpha.beta", "v1.0.0-alpha.1", 1},
		{"v1.0.0-beta", "v1.0.0-alpha", 1},
		{"dev", "v0.1.0", 0},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsReleaseAndNewer(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.1.0": true, "v0.2.0-rc.1": true, "dev": false, "v0.1.0-3-gabc1234": false,
		"v0.1.0-dirty": false, "v0.1.0-3-gabc1234-dirty": false, "v01.1.0": false, "": false,
	} {
		if got := IsRelease(v); got != want {
			t.Errorf("IsRelease(%q) = %v", v, got)
		}
	}
	if !Newer("v0.1.1", "v0.1.0") || Newer("v0.1.0", "v0.1.1") || Newer("v0.1.0", "v0.1.0") {
		t.Error("Newer: plain order")
	}
	if Newer("v0.2.0", "v0.1.0-3-gabc1234") || Newer("v0.2.0", "dev") {
		t.Error("a development build must never update")
	}
}

func TestManifestSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: "v0.1.1", Asset: ThisAsset(), Size: 10, SHA256: strings.Repeat("ab", 32)}
	body, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	sig := Sign(priv, body)
	got, err := VerifyManifest(pub, body, sig)
	if err != nil || got != m {
		t.Fatalf("verify = %+v, %v", got, err)
	}
	tampered := []byte(strings.Replace(string(body), "v0.1.1", "v0.1.2", 1))
	if _, err := VerifyManifest(pub, tampered, sig); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered manifest: %v", err)
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyManifest(otherPub, body, sig); !errors.Is(err, ErrSignature) {
		t.Fatalf("other key: %v", err)
	}
	if _, err := VerifyManifest(pub, body, []byte("garbage")); !errors.Is(err, ErrSignature) {
		t.Fatalf("garbage signature: %v", err)
	}
	if _, err := PublicKey(); err != nil {
		t.Fatalf("embedded key: %v", err)
	}
}
