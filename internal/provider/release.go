package provider

import "context"

// Release is a GitHub release (draft or published) of a code repository.
type Release struct {
	ID         int64          `json:"id"`
	TagName    string         `json:"tagName"`
	Name       string         `json:"name"`
	HTMLURL    string         `json:"htmlUrl"`
	UploadURL  string         `json:"uploadUrl"` // URI template, e.g. https://uploads.github.com/repos/o/r/releases/1/assets{?name,label}
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []ReleaseAsset `json:"assets"`
}

// Asset returns the release asset named name.
func (r Release) Asset(name string) (ReleaseAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ReleaseAsset{}, false
}

// ReleaseAsset is a file attached to a release. State is "uploaded" once the
// upload completed ("starter" while it is partial); Digest is "sha256:<hex>"
// when the platform reports it.
type ReleaseAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	State              string `json:"state"`
	Digest             string `json:"digest,omitempty"`
	BrowserDownloadURL string `json:"browserDownloadUrl"`
}

// NewRelease describes a release to create for tag TagName (created from
// TargetCommitish when the tag does not exist yet).
type NewRelease struct {
	TagName         string
	TargetCommitish string
	Name            string
	Body            string
	Draft           bool
	Prerelease      bool
}

// Releaser creates repository releases and uploads their assets (GitHub).
// repo is "owner/name".
type Releaser interface {
	// GetReleaseByTag returns the release for tag; found is false when there
	// is none (any other failure is an error).
	GetReleaseByTag(ctx context.Context, repo, tag string) (rel Release, found bool, err error)
	CreateRelease(ctx context.Context, repo string, r NewRelease) (Release, error)
	// UploadReleaseAsset streams the file at path to rel's upload URL as
	// asset name.
	UploadReleaseAsset(ctx context.Context, rel Release, name, path, contentType string) (ReleaseAsset, error)
}
