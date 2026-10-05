package release

import (
	"archive/zip"
	"context"
	"crypto/md5"  //nolint:gosec // Nexus reports md5; not used for security
	"crypto/sha1" //nolint:gosec // Factorio reports sha1; not used for security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/release/source"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// copyArtifact copies the built archive to dst and hashes it.
func copyArtifact(src, dst string) (Artifact, error) {
	in, err := os.Open(src) //nolint:gosec // G304: the profile's build output
	if err != nil {
		return Artifact{}, fmt.Errorf("build output: %w", err)
	}
	defer func() { _ = in.Close() }()
	if st, err := in.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return Artifact{}, fmt.Errorf("build output %s is not a non-empty file", filepath.Base(src))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return Artifact{}, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: data\release\<run>
	if err != nil {
		return Artifact{}, err
	}
	h256, h1, h5 := sha256.New(), sha1.New(), md5.New() //nolint:gosec // see imports
	n, err := io.Copy(io.MultiWriter(out, h256, h1, h5), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Path: dst, Name: filepath.Base(dst), Size: n, SHA256: hex.EncodeToString(h256.Sum(nil)),
		SHA1: hex.EncodeToString(h1.Sum(nil)), MD5: hex.EncodeToString(h5.Sum(nil))}, nil
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // G304: the run's artifact
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// factorioLayout: the manifest's version comes from info.json or a target is on the Factorio portal.
func factorioLayout(m *Manifest) bool {
	if m.Profile.Version.Kind == source.KindFactorioInfo {
		return true
	}
	for _, t := range m.Targets {
		if t.Platform == "factorio" {
			return true
		}
	}
	return false
}

// archiveCheck: the archive opens; Factorio layout = one top folder with an
// info.json whose name and version match; size within ±50 % of the previous
// finished release of the project.
func (e *Engine) archiveCheck(ctx context.Context, c *rc) localResult {
	if c.art == nil {
		return localResult{err: errors.New("no built artifact")}
	}
	note, err := e.checkArchive(ctx, *c.art, c.m, c.run.ProjectID)
	if err != nil {
		return localResult{hold: HeldArchive, err: err}
	}
	return localResult{ref: note}
}

// checkArchive is the archive check of art (release step and full dry run).
func (e *Engine) checkArchive(ctx context.Context, art Artifact, m *Manifest, projectID int64) (string, error) {
	zr, err := zip.OpenReader(art.Path)
	if err != nil {
		return "", fmt.Errorf("the archive does not open: %w", err)
	}
	defer func() { _ = zr.Close() }()
	notes := []string{}
	if factorioLayout(m) {
		if err := checkFactorioZip(&zr.Reader, m); err != nil {
			return "", err
		}
		notes = append(notes, "factorio layout ok")
	} else {
		notes = append(notes, "no layout check for this version source")
	}
	if prev := e.previousSize(ctx, projectID); prev > 0 {
		if art.Size*2 < prev || art.Size*2 > prev*3 {
			return "", fmt.Errorf("archive size %d is outside ±50%% of the previous release (%d)", art.Size, prev)
		}
		notes = append(notes, fmt.Sprintf("size %d vs previous %d", art.Size, prev))
	}
	return strings.Join(notes, "; "), nil
}

func checkFactorioZip(zr *zip.Reader, m *Manifest) error {
	tops := map[string]bool{}
	var info *zip.File
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		top, rest, nested := strings.Cut(name, "/")
		if !nested {
			return fmt.Errorf("%s is at the archive root: a Factorio mod has one top folder", name)
		}
		tops[top] = true
		if rest == "info.json" {
			info = f
		}
	}
	if len(tops) != 1 {
		return fmt.Errorf("the archive has %d top folders, a Factorio mod has one", len(tops))
	}
	if info == nil {
		return errors.New("no info.json in the archive's top folder")
	}
	rc, err := info.Open()
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	_ = rc.Close()
	if err != nil {
		return err
	}
	var v struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("info.json: %w", err)
	}
	if v.Version != m.Version {
		return fmt.Errorf("info.json in the archive says %s, the release is %s", v.Version, m.Version)
	}
	for _, t := range m.Targets {
		if t.Platform == "factorio" && v.Name != t.ExternalID {
			return fmt.Errorf("info.json name %q is not the portal mod %q", v.Name, t.ExternalID)
		}
	}
	if m.ModName != "" && v.Name != m.ModName {
		return fmt.Errorf("info.json name %q is not %q", v.Name, m.ModName)
	}
	return nil
}

// previousSize is the artifact size of the project's last finished release (0 = none).
func (e *Engine) previousSize(ctx context.Context, projectID int64) int64 {
	runs, err := e.d.Store.Runs(ctx, store.RunFilter{ProjectID: projectID, Kind: store.RunKindRelease, State: store.RunDone, Limit: 1})
	if err != nil || len(runs) == 0 {
		return 0
	}
	steps, err := e.d.Store.RunSteps(ctx, runs[0].ID)
	if err != nil {
		return 0
	}
	if a := artifactOf(steps); a != nil {
		return a.Size
	}
	return 0
}
