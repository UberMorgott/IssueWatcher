package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OldPath is where a swap parks the replaced executable: ".<name>.old" next
// to it (locked until its process exits; the new process deletes it).
func OldPath(exe string) string {
	dir, base := filepath.Split(exe)
	return filepath.Join(dir, "."+base+".old")
}

// NewPath is the verified download waiting to be swapped in: ".<name>.new".
func NewPath(exe string) string {
	dir, base := filepath.Split(exe)
	return filepath.Join(dir, "."+base+".new")
}

// rename is os.Rename; tests replace it to fail one step.
var rename = os.Rename

// Progress reports a running download: done of total bytes.
type Progress func(done, total int64)

// Prepare downloads rel's executable for this platform next to exe as
// NewPath(exe) and verifies it: the manifest signature (pub), version (equal
// to the tag, newer than current, current ≥ minVersion), size and SHA-256
// (and GitHub's asset digest when present). On any failure NewPath is removed
// and the running executable is untouched.
func (s Source) Prepare(ctx context.Context, rel *Release, pub ed25519.PublicKey, current, exe string, progress Progress) (Manifest, error) {
	m, err := s.manifest(ctx, rel, pub)
	if err != nil {
		return m, err
	}
	switch {
	case m.Version != rel.Tag:
		return m, fmt.Errorf("signed manifest is for %s, not the release %s", m.Version, rel.Tag)
	case !Newer(m.Version, current):
		return m, fmt.Errorf("refusing %s: not newer than the running %s", m.Version, current)
	case m.MinVersion != "" && Compare(current, m.MinVersion) < 0:
		return m, fmt.Errorf("%s needs %s or later installed first", m.Version, m.MinVersion)
	case m.Asset != ThisAsset():
		return m, fmt.Errorf("manifest names %s, this platform needs %s", m.Asset, ThisAsset())
	}
	a, ok := rel.asset(m.Asset)
	if !ok {
		return m, fmt.Errorf("release %s has no %s", rel.Tag, m.Asset)
	}
	if a.Size != 0 && a.Size != m.Size {
		return m, fmt.Errorf("%s: GitHub lists %d bytes, the signed manifest %d", m.Asset, a.Size, m.Size)
	}
	if d, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && !strings.EqualFold(d, m.SHA256) {
		return m, fmt.Errorf("%s: GitHub digest %s differs from the signed manifest", m.Asset, d)
	}
	if err := s.download(ctx, a.URL, NewPath(exe), m, progress); err != nil {
		_ = os.Remove(NewPath(exe))
		return m, err
	}
	return m, nil
}

// manifest fetches and verifies the release's manifest.json.
func (s Source) manifest(ctx context.Context, rel *Release, pub ed25519.PublicKey) (Manifest, error) {
	ma, ok1 := rel.asset(ManifestAsset)
	sa, ok2 := rel.asset(SignatureAsset)
	if !ok1 || !ok2 {
		return Manifest{}, fmt.Errorf("release %s has no signed manifest: refusing an unverifiable update", rel.Tag)
	}
	body, err := s.fetchSmall(ctx, ma.URL)
	if err != nil {
		return Manifest{}, err
	}
	sig, err := s.fetchSmall(ctx, sa.URL)
	if err != nil {
		return Manifest{}, err
	}
	return VerifyManifest(pub, body, sig)
}

func (s Source) checkURL(u string) error {
	if s.official() && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("refusing non-HTTPS release asset URL %q", u)
	}
	return nil
}

func (s Source) get(ctx context.Context, u string) (*http.Response, error) {
	if err := s.checkURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "issuewatcher-selfupdate")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: HTTP %d", u, resp.StatusCode)
	}
	return resp, nil
}

func (s Source) fetchSmall(ctx context.Context, u string) ([]byte, error) {
	resp, err := s.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	return b, nil
}

// download streams u into path, hashing as it goes; at most m.Size+1 bytes
// are read, so an oversized body fails without filling the disk.
func (s Source) download(ctx context.Context, u, path string, m Manifest, progress Progress) error {
	resp, err := s.get(ctx, u)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755) //nolint:gosec // G302/G304: our own exe folder; an exe must be executable
	if err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	h := sha256.New()
	src := io.LimitReader(resp.Body, m.Size+1)
	if progress != nil {
		src = &counter{r: src, total: m.Size, report: progress}
	}
	n, cerr := io.Copy(io.MultiWriter(f, h), src)
	serr := f.Sync()
	if err := errors.Join(cerr, serr, f.Close()); err != nil {
		return fmt.Errorf("download %s: %w", m.Asset, err)
	}
	if n != m.Size {
		return fmt.Errorf("download %s: got %d bytes, the signed manifest says %d", m.Asset, n, m.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		return fmt.Errorf("checksum mismatch for %s: sha256 %s, the signed manifest says %s", m.Asset, got, m.SHA256)
	}
	return nil
}

type counter struct {
	r      io.Reader
	done   int64
	total  int64
	report Progress
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.done += int64(n)
		c.report(c.done, c.total)
	}
	return n, err
}

// Swap moves the running exe to OldPath and the verified NewPath into its
// place. A failure of the second rename puts the old exe back.
func Swap(exe string) error {
	old := OldPath(exe)
	if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove leftover %s: %w", filepath.Base(old), err)
	}
	if err := rename(exe, old); err != nil {
		return fmt.Errorf("move %s aside: %w", filepath.Base(exe), err)
	}
	if err := rename(NewPath(exe), exe); err != nil {
		if rerr := rename(old, exe); rerr != nil {
			return fmt.Errorf("install new %s: %w (and restoring the old one failed: %w)", filepath.Base(exe), err, rerr)
		}
		return fmt.Errorf("install new %s: %w", filepath.Base(exe), err)
	}
	return nil
}

// Restore undoes Swap: the new exe (possibly running) goes back to NewPath and
// the old one to exe. The caller removes NewPath once nothing runs it.
func Restore(exe string) error {
	old := OldPath(exe)
	if _, err := os.Stat(old); err != nil {
		return fmt.Errorf("nothing to roll back to: %w", err)
	}
	_ = os.Remove(NewPath(exe))
	if err := rename(exe, NewPath(exe)); err != nil {
		return fmt.Errorf("move the new %s aside: %w", filepath.Base(exe), err)
	}
	if err := rename(old, exe); err != nil {
		_ = rename(NewPath(exe), exe)
		return fmt.Errorf("restore the old %s: %w", filepath.Base(exe), err)
	}
	return nil
}

// Cleanup deletes OldPath and NewPath of exe, retrying while a just-exited
// process still holds them (tries × delay bound the wait). It returns the
// first file that could not be removed.
func Cleanup(ctx context.Context, exe string, tries int, delay time.Duration) error {
	var err error
	for i := range max(tries, 1) {
		err = nil
		for _, p := range []string{OldPath(exe), NewPath(exe)} {
			if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && err == nil {
				err = rerr
			}
		}
		if err == nil || i == tries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
	}
	return err
}
