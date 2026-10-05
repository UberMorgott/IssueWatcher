package curseforge

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: an archive fingerprint for the result, not security
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/secret"
)

// Publishing over the official CurseForge upload API
// (https://support.curseforge.com/support/solutions/articles/9000197321):
// POST https://www.curseforge.com/api/projects/{id}/upload-file, multipart
// fields "metadata" (JSON) and "file", header X-Api-Token (the author's API
// token from authors.curseforge.com → API tokens; never in the URL). The
// www host serves every game (per-game hosts such as hytale.curseforge.com
// answer 404); game versions come from GET /api/game/{slug}/versions.
//
// A new file waits for moderation before it is public. Public files are read
// from CFWidget (approved files only); the app's own uploads are kept in a
// journal so a probe sees a file still in moderation (Pending) and an upload
// whose answer was lost is reported as unknown instead of absent.

const (
	uploadSite    = "https://www.curseforge.com"
	tokenFile     = "curseforge-upload.json" //nolint:gosec // G101: a file name, not a credential
	journalFile   = "curseforge-uploads.json"
	userAgent     = "IssueWatcher (+https://github.com/UberMorgott/issuewatcher)"
	checkSlug     = "minecraft" // any game: the version list only needs a valid token
	maxModeration = 72 * time.Hour
)

// ErrNoToken: no upload token is stored. ErrBadToken: the API refused it.
var (
	ErrNoToken  = fmt.Errorf("curseforge: no upload token (Settings › Платформы › CurseForge): %w", provider.ErrNoUploadAuth)
	ErrBadToken = fmt.Errorf("curseforge: upload token refused: %w", provider.ErrUploadAuthRefused)
)

// errUploadUnknown: an upload of this version was sent and its answer lost.
var errUploadUnknown = errors.New("curseforge: an earlier upload of this version has no recorded answer")

// UploadOptions configures the Uploader.
type UploadOptions struct {
	Dir    string       // data\secrets: the token (DPAPI) and the upload journal
	HTTP   *http.Client // default http.DefaultClient
	Site   string       // https://www.curseforge.com override (tests)
	Widget string       // https://api.cfwidget.com override (tests)
	Now    func() time.Time
}

// Uploader publishes new files of CurseForge projects (provider.Publisher).
type Uploader struct {
	opts UploadOptions
	mu   sync.Mutex // token file
	jmu  sync.Mutex // journal file
}

var _ provider.Publisher = (*Uploader)(nil)

// NewUploader returns the uploader.
func NewUploader(opts UploadOptions) *Uploader {
	if opts.HTTP == nil {
		opts.HTTP = http.DefaultClient
	}
	if opts.Site == "" {
		opts.Site = uploadSite
	}
	if opts.Widget == "" {
		opts.Widget = widgetOrigin
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Uploader{opts: opts}
}

// --- token ------------------------------------------------------------------

type tokenData struct {
	Token     string    `json:"token"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
}

// TokenStatus is the non-secret view of the token.
type TokenStatus struct {
	HasToken  bool   `json:"hasToken"`
	CheckedAt string `json:"checkedAt,omitempty"`
}

func (d tokenData) status() TokenStatus {
	st := TokenStatus{HasToken: d.Token != ""}
	if !d.CheckedAt.IsZero() {
		st.CheckedAt = d.CheckedAt.UTC().Format(time.RFC3339)
	}
	return st
}

func (u *Uploader) tokenPath() string { return filepath.Join(u.opts.Dir, tokenFile) }

func (u *Uploader) load() (tokenData, error) {
	var d tokenData
	err := secret.ReadProtectedJSON(u.tokenPath(), &d)
	if errors.Is(err, secret.ErrNotFound) {
		return tokenData{}, nil
	}
	return d, err
}

// Status reports whether a token is stored.
func (u *Uploader) Status() (TokenStatus, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	d, err := u.load()
	return d.status(), err
}

func (u *Uploader) token() (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	d, err := u.load()
	if err != nil {
		return "", err
	}
	if d.Token == "" {
		return "", ErrNoToken
	}
	return d.Token, nil
}

var tokenRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,128}$`)

// Save validates token with a read-only call and stores it; "" removes the
// stored token. A refused token is not stored (ErrBadToken).
func (u *Uploader) Save(ctx context.Context, token string) (TokenStatus, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		u.mu.Lock()
		defer u.mu.Unlock()
		return TokenStatus{}, secret.Remove(u.tokenPath())
	}
	if !tokenRe.MatchString(token) {
		return TokenStatus{}, fmt.Errorf("%w: unexpected characters", ErrBadToken)
	}
	if err := u.validate(ctx, token); err != nil {
		return TokenStatus{}, err
	}
	d := tokenData{Token: token, CheckedAt: u.opts.Now().UTC()}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := secret.WriteProtectedJSON(u.tokenPath(), d); err != nil {
		return TokenStatus{}, err
	}
	return d.status(), nil
}

// Check re-validates the stored token (ErrNoToken when none).
func (u *Uploader) Check(ctx context.Context) (TokenStatus, error) {
	token, err := u.token()
	if err != nil {
		return TokenStatus{}, err
	}
	if err := u.validate(ctx, token); err != nil {
		return TokenStatus{}, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	d, err := u.load()
	if err != nil || d.Token != token { // replaced meanwhile: report it unchanged
		return d.status(), err
	}
	d.CheckedAt = u.opts.Now().UTC()
	return d.status(), secret.WriteProtectedJSON(u.tokenPath(), d)
}

func (u *Uploader) validate(ctx context.Context, token string) error {
	_, err := u.gameVersions(ctx, token, checkSlug)
	return err
}

// --- HTTP -------------------------------------------------------------------

// apiError is the upload API's error answer.
type apiError struct {
	ErrorCode    any    `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// call sends req with the token and reads at most 1 MiB of the answer.
func (u *Uploader) call(req *http.Request, token, what string) ([]byte, error) {
	req.Header.Set("X-Api-Token", token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	res, err := u.opts.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("curseforge: %s: %s", what, strings.ReplaceAll(err.Error(), token, "***"))
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	switch res.StatusCode {
	case http.StatusOK:
		return b, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w (%s: HTTP %d)", ErrBadToken, what, res.StatusCode)
	}
	var ae apiError
	if json.Unmarshal(b, &ae) == nil && ae.ErrorMessage != "" {
		return nil, &statusError{status: res.StatusCode, msg: fmt.Sprintf("curseforge: %s: %v: %s", what, ae.ErrorCode, ae.ErrorMessage)}
	}
	return nil, &statusError{status: res.StatusCode, msg: fmt.Sprintf("curseforge: %s: HTTP %d: %s", what, res.StatusCode, snippet(b))}
}

// statusError is a non-auth error answer.
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

// gameVersion is one entry of GET /api/game/{slug}/versions.
type gameVersion struct {
	ID                int    `json:"id"`
	GameVersionTypeID int    `json:"gameVersionTypeID"`
	Name              string `json:"name"`
	Slug              string `json:"slug"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9-]+$`)

func (u *Uploader) gameVersions(ctx context.Context, token, slug string) ([]gameVersion, error) {
	if !slugRe.MatchString(slug) {
		return nil, fmt.Errorf("curseforge: bad game slug %q", slug)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.opts.Site+"/api/game/"+slug+"/versions", nil)
	if err != nil {
		return nil, err
	}
	b, err := u.call(req, token, "game versions")
	if err != nil {
		return nil, err
	}
	var vs []gameVersion
	if err := json.Unmarshal(b, &vs); err != nil {
		return nil, fmt.Errorf("curseforge: game versions: unexpected answer")
	}
	return vs, nil
}

// widgetProject is the part of CFWidget's project answer publishing reads.
type widgetProject struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Game  string `json:"game"`
	URLs  struct {
		CurseForge string `json:"curseforge"`
	} `json:"urls"`
	Files []widgetFile `json:"files"`
}

type widgetFile struct {
	ID         int      `json:"id"`
	Display    string   `json:"display"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	FileSize   int64    `json:"filesize"`
	Versions   []string `json:"versions"`
	UploadedAt string   `json:"uploaded_at"`
}

func (u *Uploader) widget(ctx context.Context, mod int) (widgetProject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.opts.Widget+"/"+strconv.Itoa(mod), nil)
	if err != nil {
		return widgetProject{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	res, err := u.opts.HTTP.Do(req)
	if err != nil {
		return widgetProject{}, fmt.Errorf("curseforge: project %d: %w", mod, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		return widgetProject{}, fmt.Errorf("curseforge: project %d: HTTP %d", mod, res.StatusCode)
	}
	var p widgetProject
	if err := json.Unmarshal(b, &p); err != nil || p.ID != mod {
		return widgetProject{}, fmt.Errorf("curseforge: project %d: unexpected answer", mod)
	}
	return p, nil
}

// --- journal ----------------------------------------------------------------

// journalEntry is one upload the app sent: FileID is empty while the answer
// is outstanding (or was lost).
type journalEntry struct {
	Project int       `json:"project"`
	Version string    `json:"version"`
	FileID  int       `json:"fileId,omitempty"`
	Name    string    `json:"name,omitempty"`
	Size    int64     `json:"size"`
	MD5     string    `json:"md5"`
	SentAt  time.Time `json:"sentAt"`
}

func (u *Uploader) journalPath() string { return filepath.Join(u.opts.Dir, journalFile) }

func (u *Uploader) journal() ([]journalEntry, error) {
	var j []journalEntry
	err := secret.ReadJSON(u.journalPath(), &j)
	if errors.Is(err, secret.ErrNotFound) {
		return nil, nil
	}
	return j, err
}

// updateJournal applies f under the lock and drops entries older than the
// moderation limit.
func (u *Uploader) updateJournal(f func([]journalEntry) []journalEntry) error {
	u.jmu.Lock()
	defer u.jmu.Unlock()
	j, err := u.journal()
	if err != nil {
		return err
	}
	j = f(j)
	cut := u.opts.Now().Add(-maxModeration)
	j = slices.DeleteFunc(j, func(e journalEntry) bool { return e.SentAt.Before(cut) })
	return secret.WriteJSON(u.journalPath(), j)
}

func (u *Uploader) entries(mod int) ([]journalEntry, error) {
	u.jmu.Lock()
	defer u.jmu.Unlock()
	j, err := u.journal()
	if err != nil {
		return nil, err
	}
	cut := u.opts.Now().Add(-maxModeration)
	return slices.DeleteFunc(j, func(e journalEntry) bool { return e.Project != mod || e.SentAt.Before(cut) }), nil
}

// --- publisher --------------------------------------------------------------

// fileVersionRe finds the version in a file's display name or file name.
var fileVersionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:-[0-9A-Za-z.]+)?`)

func fileVersion(f widgetFile) string {
	for _, s := range []string{f.Display, strings.TrimSuffix(f.Name, filepath.Ext(f.Name))} {
		if m := fileVersionRe.FindAllString(s, -1); len(m) > 0 {
			return m[len(m)-1]
		}
	}
	return ""
}

func sameVersion(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") == strings.TrimPrefix(strings.TrimSpace(b), "v")
}

func filesURL(p widgetProject) string {
	if p.URLs.CurseForge != "" {
		return strings.TrimRight(p.URLs.CurseForge, "/") + "/files"
	}
	return fmt.Sprintf("%s/projects/%d", siteOrigin, p.ID)
}

// targets lists the project's public files (approved) plus the app's own
// uploads still in moderation (Pending), oldest first.
func (u *Uploader) targets(ctx context.Context, mod int) (provider.PublishTargets, widgetProject, error) {
	p, err := u.widget(ctx, mod)
	if err != nil {
		return provider.PublishTargets{}, p, err
	}
	files := slices.Clone(p.Files)
	slices.SortFunc(files, func(a, b widgetFile) int { return strings.Compare(a.UploadedAt, b.UploadedAt) })
	f := provider.PublishFile{ID: strconv.Itoa(mod), Name: p.Title, Active: true}
	public := map[int]bool{}
	for _, w := range files {
		public[w.ID] = true
		f.Versions = append(f.Versions, provider.PublishVersion{ID: strconv.Itoa(w.ID), Name: w.Display, Version: fileVersion(w),
			Category: w.Type, UploadedAt: w.UploadedAt})
		f.LastUploadedAt = w.UploadedAt
	}
	own, err := u.entries(mod)
	if err != nil {
		return provider.PublishTargets{}, p, err
	}
	for _, e := range own {
		listed := slices.ContainsFunc(f.Versions, func(v provider.PublishVersion) bool { return sameVersion(v.Version, e.Version) })
		switch {
		case listed || public[e.FileID]:
		case e.FileID == 0:
			return provider.PublishTargets{}, p, fmt.Errorf("%w: %s sent %s; check %s, it is shown there once moderated",
				errUploadUnknown, e.Version, e.SentAt.UTC().Format(time.RFC3339), filesURL(p))
		default:
			f.Versions = append(f.Versions, provider.PublishVersion{ID: strconv.Itoa(e.FileID), Name: e.Name, Version: e.Version,
				UploadedAt: e.SentAt.UTC().Format(time.RFC3339), Pending: true})
		}
	}
	f.VersionsCount = len(f.Versions)
	return provider.PublishTargets{ModUID: strconv.Itoa(mod), ModName: p.Title, FilesURL: filesURL(p), Files: []provider.PublishFile{f}}, p, nil
}

// PublishTargets implements provider.Publisher: a project is one file whose
// versions are its files. Without an upload token it reports ErrNoToken (the
// plan shows it).
func (u *Uploader) PublishTargets(ctx context.Context, project provider.Project) (provider.PublishTargets, error) {
	mod, err := modID(project.ExternalID)
	if err != nil {
		return provider.PublishTargets{}, err
	}
	if _, err := u.token(); err != nil {
		return provider.PublishTargets{}, err
	}
	pt, _, err := u.targets(ctx, mod)
	return pt, err
}

var releaseTypes = []string{"release", "beta", "alpha"}

var versionRe = regexp.MustCompile(`^v?\d+(?:\.\d+){1,3}(?:[-+][0-9A-Za-z.-]+)?$`)

func badPublish(format string, a ...any) error {
	return fmt.Errorf("%w: %s", provider.ErrBadPublish, fmt.Sprintf(format, a...))
}

// CheckPublish implements provider.Publisher (no network): a new file of the
// project from a local file.
func (u *Uploader) CheckPublish(project provider.Project, req provider.PublishRequest) error {
	mod, err := modID(project.ExternalID)
	switch {
	case err != nil || mod <= 0:
		return badPublish("project id %q is not a CurseForge project id", project.ExternalID)
	case req.NewFile:
		return badPublish("newFile: every CurseForge upload is a new file of the project")
	case req.FileID != "" && req.FileID != strconv.Itoa(mod):
		return badPublish("fileId: the project's only target is %d", mod)
	case req.UploadID != "":
		return badPublish("uploadId: the CurseForge upload API has no reusable uploads; publish from path")
	case req.Path == "":
		return badPublish("path: pick the file to upload")
	case !filepath.IsAbs(req.Path):
		return badPublish("path must be absolute")
	case !versionRe.MatchString(req.Version):
		return badPublish("version: like 1.2.3")
	case req.ReleaseType != "" && !slices.Contains(releaseTypes, req.ReleaseType):
		return badPublish("releaseType: release, beta or alpha")
	case len(req.GameVersions) > 50:
		return badPublish("gameVersions: at most 50")
	}
	st, err := os.Stat(req.Path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return badPublish("path: %s is not a non-empty file", filepath.Base(req.Path))
	}
	return nil
}

// uploadMetadata is the upload-file "metadata" field.
type uploadMetadata struct {
	Changelog     string `json:"changelog"`
	ChangelogType string `json:"changelogType"`
	DisplayName   string `json:"displayName"`
	GameVersions  []int  `json:"gameVersions"`
	ReleaseType   string `json:"releaseType"`
}

// displayName is the file's title: the name with the version.
func displayName(name, version string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return version
	case strings.Contains(name, strings.TrimPrefix(version, "v")):
		return name
	}
	return name + " " + version
}

// resolveVersions maps the requested game versions (ids or names; none = the
// latest public file's) to ids of the game's version list.
func resolveVersions(want []string, p widgetProject, list []gameVersion) ([]int, error) {
	if len(want) == 0 {
		var last widgetFile
		for _, f := range p.Files {
			if f.UploadedAt > last.UploadedAt {
				last = f
			}
		}
		want = last.Versions
		if len(want) == 0 {
			return nil, badPublish("gameVersions: the project has no file to copy them from; set them in the publish profile")
		}
	}
	var ids []int
	for _, w := range want {
		w = strings.TrimSpace(w)
		i := slices.IndexFunc(list, func(v gameVersion) bool {
			return strconv.Itoa(v.ID) == w || strings.EqualFold(v.Name, w) || strings.EqualFold(v.Slug, w)
		})
		if i < 0 {
			return nil, badPublish("gameVersions: %q is not a game version of %s", w, p.Game)
		}
		if !slices.Contains(ids, list[i].ID) {
			ids = append(ids, list[i].ID)
		}
	}
	return ids, nil
}

func fileMD5(p string) (string, int64, error) {
	f, err := os.Open(p) //nolint:gosec // G304: the archive the user picked
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := md5.New() //nolint:gosec // G401: fingerprint only
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Publish implements provider.Publisher. The upload-file POST is sent at
// most once: it is journalled before it is sent, and an answer lost after
// sending makes later probes report the outcome as unknown.
func (u *Uploader) Publish(ctx context.Context, project provider.Project, req provider.PublishRequest, progress func(provider.PublishProgress)) (provider.PublishResult, error) {
	if progress == nil {
		progress = func(provider.PublishProgress) {}
	}
	if err := u.CheckPublish(project, req); err != nil {
		return provider.PublishResult{}, err
	}
	mod, _ := modID(project.ExternalID)
	res := provider.PublishResult{DryRun: req.DryRun, FileID: strconv.Itoa(mod)}
	fail := func(stage string, err error) (provider.PublishResult, error) {
		return res, &provider.PublishError{Stage: stage, Err: err}
	}

	progress(provider.PublishProgress{Stage: provider.StageResolve})
	pt, wp, err := u.targets(ctx, mod)
	if err != nil {
		return fail(provider.StageResolve, err)
	}
	res.FilesURL = pt.FilesURL
	for _, v := range pt.Files[0].Versions {
		if sameVersion(v.Version, req.Version) {
			return res, badPublish("version %s of project %d is already uploaded (file %s)", req.Version, mod, v.ID)
		}
	}
	progress(provider.PublishProgress{Stage: provider.StageHash})
	if res.MD5, res.Size, err = fileMD5(req.Path); err != nil {
		return fail(provider.StageHash, err)
	}
	res.FileName = filepath.Base(req.Path)
	meta := uploadMetadata{Changelog: req.Changelog, ChangelogType: "markdown", DisplayName: displayName(req.Name, req.Version),
		ReleaseType: cmp(req.ReleaseType, "release")}
	if strings.TrimSpace(meta.Changelog) == "" {
		meta.Changelog = "Version " + req.Version
	}

	token, terr := u.token()
	if req.DryRun {
		res.Plan = u.plan(mod, wp, req, res, meta, terr)
		return res, nil
	}
	if terr != nil {
		return fail(provider.StageResolve, terr)
	}
	list, err := u.gameVersions(ctx, token, wp.Game)
	if err != nil {
		return fail(provider.StageResolve, err)
	}
	if meta.GameVersions, err = resolveVersions(req.GameVersions, wp, list); err != nil {
		return res, err
	}

	entry := journalEntry{Project: mod, Version: req.Version, Name: meta.DisplayName, Size: res.Size, MD5: res.MD5, SentAt: u.opts.Now().UTC()}
	if err := u.updateJournal(func(j []journalEntry) []journalEntry { return append(j, entry) }); err != nil {
		return fail(provider.StageUpload, err)
	}
	progress(provider.PublishProgress{Stage: provider.StagePublish, Total: res.Size})
	id, err := u.upload(context.WithoutCancel(ctx), token, mod, req.Path, meta, res.Size, func(n int64) {
		progress(provider.PublishProgress{Stage: provider.StagePublish, Sent: n, Total: res.Size})
	})
	if err != nil {
		var se *statusError
		if errors.Is(err, ErrBadToken) || (errors.As(err, &se) && se.status >= 400 && se.status < 500) {
			// Refused before anything was stored: no file exists.
			_ = u.updateJournal(func(j []journalEntry) []journalEntry {
				return slices.DeleteFunc(j, func(e journalEntry) bool { return e.Project == mod && e.Version == req.Version && e.FileID == 0 })
			})
		}
		return fail(provider.StagePublish, err)
	}
	_ = u.updateJournal(func(j []journalEntry) []journalEntry {
		for i := range j {
			if j[i].Project == mod && j[i].Version == req.Version && j[i].FileID == 0 {
				j[i].FileID = id
			}
		}
		return j
	})
	res.VersionID = strconv.Itoa(id)
	return res, nil
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// countingReader reports bytes read.
type countingReader struct {
	r    io.Reader
	n    int64
	tick func(int64)
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	if n > 0 {
		c.tick(c.n)
	}
	return n, err
}

// upload sends the upload-file request: "metadata" then "file", with a
// Content-Length, and returns the new file's id.
func (u *Uploader) upload(ctx context.Context, token string, mod int, path string, meta uploadMetadata, size int64, tick func(int64)) (int, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the archive the user picked
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	mb, err := json.Marshal(meta)
	if err != nil {
		return 0, err
	}
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if err := mw.WriteField("metadata", string(mb)); err != nil {
		return 0, err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(path)))
	h.Set("Content-Type", "application/octet-stream")
	if _, err := mw.CreatePart(h); err != nil {
		return 0, err
	}
	tail := "\r\n--" + mw.Boundary() + "--\r\n"
	body := io.MultiReader(&head, &countingReader{r: io.LimitReader(f, size), tick: tick}, strings.NewReader(tail))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/projects/%d/upload-file", u.opts.Site, mod), body)
	if err != nil {
		return 0, err
	}
	req.ContentLength = int64(head.Len()) + size + int64(len(tail))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	b, err := u.call(req, token, "upload-file")
	if err != nil {
		return 0, err
	}
	var a struct {
		ID int `json:"id"`
	}
	if json.Unmarshal(b, &a) != nil || a.ID <= 0 {
		return 0, fmt.Errorf("curseforge: upload-file: answer without a file id: %s", snippet(b))
	}
	return a.ID, nil
}

// plan lists the requests a publish of req would send.
func (u *Uploader) plan(mod int, wp widgetProject, req provider.PublishRequest, res provider.PublishResult, meta uploadMetadata, terr error) []provider.PublishStep {
	auth := "X-Api-Token: <stored upload token>"
	if terr != nil {
		auth = "no upload token: Settings › Платформы › CurseForge (" + terr.Error() + ")"
	}
	versions := "the latest file's game versions"
	if len(req.GameVersions) > 0 {
		versions = strings.Join(req.GameVersions, ", ")
	}
	body := map[string]any{"changelog": meta.Changelog, "changelogType": meta.ChangelogType, "displayName": meta.DisplayName,
		"gameVersions": "ids of: " + versions, "releaseType": meta.ReleaseType}
	return []provider.PublishStep{
		{Method: http.MethodGet, URL: u.opts.Site + "/api/game/" + wp.Game + "/versions", Note: auth + "; maps game versions to ids"},
		{Method: http.MethodPost, URL: fmt.Sprintf("%s/api/projects/%d/upload-file", u.opts.Site, mod), Body: body,
			Note: fmt.Sprintf("multipart metadata + file %s (%d bytes, md5 %s); %s; sent once, then moderated by CurseForge", res.FileName, res.Size, res.MD5, auth)},
	}
}
