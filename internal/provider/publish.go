package provider

import (
	"context"
	"errors"
	"fmt"
)

// Publishing a new mod version from the app (Phase 7; Nexus first): a
// provider with Capabilities.Publish implements Publisher.

// ErrBadPublish: a publish request the platform would refuse (checked before
// anything is uploaded).
var ErrBadPublish = errors.New("bad publish request")

// ErrNoUploadAuth: the platform's upload credentials are not set up (Steam:
// no steamcmd sign-in, CurseForge: no upload token); the owner fills the form
// in Settings › Платформы. ErrUploadAuthRefused: they are set up but refused
// (expired steamcmd session, revoked token).
var (
	ErrNoUploadAuth      = errors.New("upload credentials not set up")
	ErrUploadAuthRefused = errors.New("upload credentials refused")
)

// Publisher is a provider that uploads archives as new mod file versions.
type Publisher interface {
	// PublishTargets lists the project's files and their versions.
	PublishTargets(ctx context.Context, project Project) (PublishTargets, error)
	// CheckPublish validates req without network calls (ErrBadPublish).
	CheckPublish(project Project, req PublishRequest) error
	// Publish uploads req.Path (or reuses req.UploadID) and publishes it. The
	// publish call itself is sent at most once: a failure after the upload
	// returns a *PublishError carrying the upload id to retry with. DryRun
	// returns the planned requests and writes nothing. progress may be nil.
	Publish(ctx context.Context, project Project, req PublishRequest, progress func(PublishProgress)) (PublishResult, error)
}

// PublishTargets are the files a project can publish to.
type PublishTargets struct {
	ModUID   string        `json:"modUid"`
	ModName  string        `json:"modName"`
	FilesURL string        `json:"filesUrl"`
	Files    []PublishFile `json:"files"`
}

// PublishFile is one updatable file of a mod page and its versions (newest last).
type PublishFile struct {
	ID             string           `json:"id"`
	Name           string           `json:"name"`
	Active         bool             `json:"active"`
	VersionsCount  int              `json:"versionsCount"`
	ArchivedCount  int              `json:"archivedCount"`
	LastUploadedAt string           `json:"lastUploadedAt,omitempty"`
	Versions       []PublishVersion `json:"versions"`
}

// PublishVersion is one version of a file.
type PublishVersion struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Category   string `json:"category"`
	UploadedAt string `json:"uploadedAt"`
	Primary    bool   `json:"primary,omitempty"`
	// SHA1 is the uploaded archive's SHA-1 when the platform reports it
	// (Factorio mod portal), so a probe can tell the same file from another.
	SHA1 string `json:"sha1,omitempty"`
	// Pending: uploaded but not downloadable yet (CurseForge moderation).
	Pending bool `json:"pending,omitempty"`
}

// PublishRequest is what to publish.
type PublishRequest struct {
	// FileID is the file to add a version to; NewFile creates a new file instead.
	FileID  string `json:"fileId,omitempty"`
	NewFile bool   `json:"newFile,omitempty"`
	// Path is the local archive; UploadID reuses an upload that is already
	// available (the retry after a failed publish), Path then stays empty.
	Path     string `json:"path,omitempty"`
	UploadID string `json:"uploadId,omitempty"`

	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"` // main | optional | miscellaneous ("" = main)

	ArchivePrevious   bool   `json:"archivePrevious,omitempty"`   // new version only
	PreviousVersionID string `json:"previousVersionId,omitempty"` // new version only
	UpdateModVersion  bool   `json:"updateModVersion,omitempty"`

	PrimaryModManagerDownload *bool `json:"primaryModManagerDownload,omitempty"`
	AllowModManagerDownload   *bool `json:"allowModManagerDownload,omitempty"`
	ShowRequirementsPopUp     *bool `json:"showRequirementsPopUp,omitempty"`

	Changelog string `json:"changelog,omitempty"` // added for Version after the publish

	// Steam Workshop: the game's app id (0 = the item's consumer app). Path is
	// the content folder, or a zip that is unpacked into one.
	AppID int `json:"appId,omitempty"`
	// CurseForge: game version ids or names (none = the previous file's) and
	// release | beta | alpha ("" = release).
	GameVersions []string `json:"gameVersions,omitempty"`
	ReleaseType  string   `json:"releaseType,omitempty"`

	DryRun bool `json:"dryRun,omitempty"`
}

// PublishStep is one planned request of a dry run.
type PublishStep struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Body   any    `json:"body,omitempty"`
	Note   string `json:"note,omitempty"`
}

// PublishResult is a finished (or planned) publish.
type PublishResult struct {
	DryRun    bool          `json:"dryRun,omitempty"`
	Plan      []PublishStep `json:"plan,omitempty"`
	UploadID  string        `json:"uploadId,omitempty"`
	MD5       string        `json:"md5,omitempty"` // hex digest of the archive
	Size      int64         `json:"size,omitempty"`
	FileID    string        `json:"fileId,omitempty"`
	FileName  string        `json:"fileName,omitempty"`
	VersionID string        `json:"versionId,omitempty"`
	FilesURL  string        `json:"filesUrl,omitempty"`
	// Changelog: "" none asked, "added", or "failed: <reason>" (the version is published regardless).
	Changelog string `json:"changelog,omitempty"`
}

// Publish stages reported through PublishProgress.Stage.
const (
	StageResolve   = "resolve"   // reading the mod and its files
	StageHash      = "hash"      // reading the archive (size, md5)
	StageUpload    = "upload"    // sending the parts (Sent of Total bytes)
	StageWait      = "wait"      // finalised, waiting for the upload to become available
	StagePublish   = "publish"   // the publish call (sent once)
	StageChangelog = "changelog" // adding the changelog
)

// PublishProgress is one step of a running publish.
type PublishProgress struct {
	Stage string `json:"stage"`
	Sent  int64  `json:"sent,omitempty"`
	Total int64  `json:"total,omitempty"`
}

// PublishError is a failed publish. UploadID is set when the archive is
// already uploaded: retry with PublishRequest.UploadID, without re-uploading.
type PublishError struct {
	Stage    string
	UploadID string
	Err      error
}

func (e *PublishError) Error() string {
	if e.UploadID != "" {
		return fmt.Sprintf("publish (%s, upload %s): %v", e.Stage, e.UploadID, e.Err)
	}
	return fmt.Sprintf("publish (%s): %v", e.Stage, e.Err)
}

func (e *PublishError) Unwrap() error { return e.Err }
