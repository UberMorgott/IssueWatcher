package nexus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Publishing new versions over the v3 API (Phase 7, v3.go). The provider is
// a provider.Publisher once it has the API key store (Options.Keys).

// Field rules of CreateModFileVersionRequest / AddModChangelogEntriesRequest
// (openapi.yaml): checked before anything is uploaded.
var (
	v3NameRe    = regexp.MustCompile(`^[a-zA-Z0-9 _'().-]+$`)
	v3VersionRe = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)
	categories  = []string{"main", "optional", "miscellaneous"} // NewModFileCategory
)

const (
	maxV3Name      = 50
	maxV3Version   = 50
	maxV3Changelog = 65535
)

var _ provider.Publisher = (*Provider)(nil)

// v3Client is the upload client, nil without Options.Keys / Options.V3.
func (p *Provider) v3Client() (*v3, error) {
	if p.v3 == nil {
		return nil, fmt.Errorf("%w: publishing is not set up", ErrNoAPIKey)
	}
	return p.v3, nil
}

// PublishTargets implements provider.Publisher: the mod's files with their versions.
func (p *Provider) PublishTargets(ctx context.Context, project provider.Project) (provider.PublishTargets, error) {
	c, err := p.v3Client()
	if err != nil {
		return provider.PublishTargets{}, err
	}
	game, mod, err := parseProject(project.ExternalID)
	if err != nil {
		return provider.PublishTargets{}, err
	}
	m, err := c.mod(ctx, game, mod)
	if err != nil {
		return provider.PublishTargets{}, err
	}
	files, err := c.modFiles(ctx, m.ID)
	if err != nil {
		return provider.PublishTargets{}, err
	}
	out := provider.PublishTargets{ModUID: m.ID, FilesURL: filesURL(game, mod), Files: []provider.PublishFile{}}
	if m.Name != nil {
		out.ModName = *m.Name
	}
	for _, f := range files {
		vs, err := c.fileVersions(ctx, f.ID)
		if err != nil {
			return provider.PublishTargets{}, err
		}
		pf := provider.PublishFile{ID: f.ID, Name: f.Name, Active: f.IsActive, VersionsCount: f.VersionsCount,
			ArchivedCount: f.ArchivedCount, Versions: make([]provider.PublishVersion, 0, len(vs))}
		if f.LastFileUploadedAt != nil {
			pf.LastUploadedAt = *f.LastFileUploadedAt
		}
		for _, v := range vs {
			pf.Versions = append(pf.Versions, provider.PublishVersion{ID: v.ID, Name: v.Name, Version: v.Version,
				Category: v.Category, UploadedAt: v.UploadedAt, Primary: v.IsPrimary})
		}
		out.Files = append(out.Files, pf)
	}
	return out, nil
}

func filesURL(game string, mod int) string {
	return fmt.Sprintf("%s/%s/mods/%d?tab=files", site, game, mod)
}

func badPublish(format string, a ...any) error {
	return fmt.Errorf("%w: %s", provider.ErrBadPublish, fmt.Sprintf(format, a...))
}

// CheckPublish implements provider.Publisher (no network).
func (p *Provider) CheckPublish(project provider.Project, req provider.PublishRequest) error {
	if _, _, err := parseProject(project.ExternalID); err != nil {
		return badPublish("%v", err)
	}
	switch {
	case req.NewFile && req.FileID != "":
		return badPublish("pass fileId or newFile, not both")
	case req.NewFile && (req.ArchivePrevious || req.PreviousVersionID != ""):
		return badPublish("archivePrevious and previousVersionId apply to a new version, not a new file")
	case !req.NewFile && req.FileID == "":
		return badPublish("fileId: pick the file to add the version to")
	}
	if req.Path == "" && req.UploadID == "" {
		return badPublish("path: pick the archive to upload")
	}
	if req.Path != "" {
		if req.UploadID != "" {
			return badPublish("pass path or uploadId, not both")
		}
		if !filepath.IsAbs(req.Path) {
			return badPublish("path must be absolute")
		}
		st, err := os.Stat(req.Path)
		if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			return badPublish("path: %s is not a non-empty file", filepath.Base(req.Path))
		}
	}
	if req.Name == "" || len(req.Name) > maxV3Name || !v3NameRe.MatchString(req.Name) {
		return badPublish("name: 1-%d characters of letters, digits, space and _ ' ( ) . -", maxV3Name)
	}
	if req.Version == "" || len(req.Version) > maxV3Version || !v3VersionRe.MatchString(req.Version) {
		return badPublish("version: 1-%d characters of letters, digits, . and -", maxV3Version)
	}
	if req.Category != "" && !slices.Contains(categories, req.Category) {
		return badPublish("category: main, optional or miscellaneous")
	}
	if utf8.RuneCountInString(req.Changelog) > maxV3Changelog {
		return badPublish("changelog: at most %d characters", maxV3Changelog)
	}
	return nil
}

// versionBody is the exact POST /mod-files/{id}/versions body of req.
func versionBody(req provider.PublishRequest, uploadID string) v3VersionBody {
	b := v3VersionBody{
		UploadID: uploadID, Name: req.Name, Version: req.Version, FileCategory: cmpName(req.Category, "main"),
		PrimaryModManagerDownload: req.PrimaryModManagerDownload, AllowModManagerDownload: req.AllowModManagerDownload,
		ShowRequirementsPopUp: req.ShowRequirementsPopUp,
		UpdateModVersion:      req.UpdateModVersion, ArchiveExistingFile: req.ArchivePrevious,
	}
	if d := strings.TrimSpace(req.Description); d != "" {
		b.Description = &d
	}
	if req.PreviousVersionID != "" {
		b.PreviousVersionID = &req.PreviousVersionID
	}
	return b
}

// fileBody is the exact POST /mod-files body of req (CreateModFileRequest).
func fileBody(req provider.PublishRequest, modUID, uploadID string) v3FileBody {
	b := v3FileBody{
		UploadID: uploadID, ModID: modUID, Name: req.Name, Version: req.Version, FileCategory: cmpName(req.Category, "main"),
		PrimaryModManagerDownload: req.PrimaryModManagerDownload, AllowModManagerDownload: req.AllowModManagerDownload,
		ShowRequirementsPopUp: req.ShowRequirementsPopUp, UpdateModVersion: req.UpdateModVersion,
	}
	if d := strings.TrimSpace(req.Description); d != "" {
		b.Description = &d
	}
	return b
}

// planUploadID stands in for the upload id in a dry run's bodies.
const planUploadID = "{upload_id}"

// Publish implements provider.Publisher.
func (p *Provider) Publish(ctx context.Context, project provider.Project, req provider.PublishRequest, progress func(provider.PublishProgress)) (provider.PublishResult, error) {
	if progress == nil {
		progress = func(provider.PublishProgress) {}
	}
	if err := p.CheckPublish(project, req); err != nil {
		return provider.PublishResult{}, err
	}
	c, err := p.v3Client()
	if err != nil {
		return provider.PublishResult{}, err
	}
	game, mod, _ := parseProject(project.ExternalID)
	res := provider.PublishResult{DryRun: req.DryRun, UploadID: req.UploadID, FilesURL: filesURL(game, mod)}
	fail := func(stage string, err error) (provider.PublishResult, error) {
		return res, &provider.PublishError{Stage: stage, UploadID: res.UploadID, Err: err}
	}

	modUID := ""
	if req.Changelog != "" || req.DryRun || req.NewFile {
		progress(provider.PublishProgress{Stage: provider.StageResolve})
		m, err := c.mod(ctx, game, mod)
		if err != nil {
			return fail(provider.StageResolve, err)
		}
		modUID = m.ID
	}
	if req.Path != "" {
		progress(provider.PublishProgress{Stage: provider.StageHash})
		if res.MD5, res.Size, err = fileMD5(req.Path); err != nil {
			return fail(provider.StageHash, err)
		}
	}

	if req.DryRun {
		res.Plan = p.plan(c, req, res, modUID)
		return res, nil
	}

	if req.Path != "" {
		id, err := c.upload(ctx, req.Path, func(sent, total int64) {
			progress(provider.PublishProgress{Stage: provider.StageUpload, Sent: sent, Total: total})
		}, func() { progress(provider.PublishProgress{Stage: provider.StageWait}) })
		if ue, ok := errors.AsType[*UploadError](err); ok {
			res.UploadID = ue.UploadID
		}
		if err != nil {
			return fail(provider.StageUpload, err)
		}
		res.UploadID = id
	}

	// Sent once, and not cancelled once sent: an aborted request may still
	// publish, and a second POST would publish twice. A failure keeps the
	// upload id for a retry.
	progress(provider.PublishProgress{Stage: provider.StagePublish})
	if req.NewFile {
		nf, err := c.createFile(context.WithoutCancel(ctx), fileBody(req, modUID, res.UploadID))
		if err != nil {
			return fail(provider.StagePublish, err)
		}
		res.FileID, res.FileName = nf.ID, nf.Name
	} else {
		r, err := c.createVersion(context.WithoutCancel(ctx), req.FileID, versionBody(req, res.UploadID))
		if err != nil {
			return fail(provider.StagePublish, err)
		}
		res.FileID, res.FileName, res.VersionID = r.File.ID, r.File.Name, r.Version.ID
	}

	if req.Changelog != "" {
		progress(provider.PublishProgress{Stage: provider.StageChangelog})
		if err := c.addChangelog(context.WithoutCancel(ctx), modUID, req.Version, req.Changelog); err != nil {
			res.Changelog = "failed: " + err.Error()
		} else {
			res.Changelog = "added"
		}
	}
	return res, nil
}

// plan lists the requests a publish of req would send.
func (p *Provider) plan(c *v3, req provider.PublishRequest, res provider.PublishResult, modUID string) []provider.PublishStep {
	base := c.opts.Base
	uploadID := cmpName(req.UploadID, planUploadID)
	var steps []provider.PublishStep
	if req.Path != "" {
		steps = append(steps,
			provider.PublishStep{Method: http.MethodPost, URL: base + "/uploads/multipart",
				Body: map[string]any{"filename": filepath.Base(req.Path), "size_bytes": res.Size},
				Note: fmt.Sprintf("md5 %s", res.MD5)},
			provider.PublishStep{Method: http.MethodPut, URL: "part_presigned_urls[*]",
				Note: "one PUT per part_size_bytes slice of the archive; the ETags are collected"},
			provider.PublishStep{Method: http.MethodPost, URL: "complete_presigned_url", Note: "CompleteMultipartUpload XML of the part ETags"},
			provider.PublishStep{Method: http.MethodPost, URL: base + "/uploads/" + uploadID + "/finalise"},
			provider.PublishStep{Method: http.MethodGet, URL: base + "/uploads/" + uploadID, Note: "until state is available"},
		)
	}
	if req.NewFile {
		steps = append(steps, provider.PublishStep{Method: http.MethodPost, URL: base + "/mod-files",
			Body: fileBody(req, modUID, uploadID), Note: "sent once"})
	} else {
		steps = append(steps, provider.PublishStep{Method: http.MethodPost, URL: base + "/mod-files/" + url.PathEscape(req.FileID) + "/versions",
			Body: versionBody(req, uploadID), Note: "sent once"})
	}
	if req.Changelog != "" {
		steps = append(steps, provider.PublishStep{Method: http.MethodPost, URL: base + "/mods/" + url.PathEscape(modUID) + "/changelogs",
			Body: map[string]string{"version": req.Version, "changelog": req.Changelog}})
	}
	return steps
}
