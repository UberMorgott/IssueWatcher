package factorio

import (
	"archive/zip"
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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

// Publishing a new release over the mod upload API
// (https://wiki.factorio.com/Mod_upload_API): POST init_upload {mod} with the
// API key → upload_url, then POST the archive as multipart field "file" to it
// (that second call publishes the release; it is sent once). The release's
// version, dependencies and changelog come from the archive itself
// (info.json, changelog.txt), so the archive is checked against the request
// before anything is sent.

const initUploadPath = "/api/v2/mods/releases/init_upload"

var _ provider.Publisher = (*Provider)(nil)

// versionRe is a Factorio mod version: three numbers 0-65535.
var versionRe = regexp.MustCompile(`^(\d{1,5})\.(\d{1,5})\.(\d{1,5})$`)

func validVersion(v string) bool {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	for _, p := range m[1:] {
		if n, err := strconv.Atoi(p); err != nil || n > 65535 {
			return false
		}
	}
	return true
}

func badPublish(format string, a ...any) error {
	return fmt.Errorf("%w: %s", provider.ErrBadPublish, fmt.Sprintf(format, a...))
}

func downloadsURL(mod string) string {
	return site + "/mod/" + url.PathEscape(mod) + "/downloads"
}

// portalMod is the part of GET /api/mods/{name} publishing reads.
type portalMod struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	Owner    string `json:"owner"`
	Releases []struct {
		Version    string `json:"version"`
		ReleasedAt string `json:"released_at"`
		FileName   string `json:"file_name"`
	} `json:"releases"`
}

func (m portalMod) has(version string) bool {
	for _, r := range m.Releases {
		if r.Version == version {
			return true
		}
	}
	return false
}

func (p *Provider) portalMod(ctx context.Context, mod string) (portalMod, error) {
	body, err := p.get(ctx, "/api/mods/"+url.PathEscape(mod), nil)
	if err != nil {
		return portalMod{}, err
	}
	var m portalMod
	if err := json.Unmarshal([]byte(body), &m); err != nil || m.Name == "" {
		return portalMod{}, fmt.Errorf("factorio: /api/mods/%s: unexpected answer", mod)
	}
	return m, nil
}

// PublishTargets implements provider.Publisher: a mod is one file whose
// versions are its releases.
func (p *Provider) PublishTargets(ctx context.Context, project provider.Project) (provider.PublishTargets, error) {
	mod := project.ExternalID
	m, err := p.portalMod(ctx, mod)
	if err != nil {
		return provider.PublishTargets{}, err
	}
	f := provider.PublishFile{ID: m.Name, Name: m.Title, Active: true, VersionsCount: len(m.Releases),
		Versions: make([]provider.PublishVersion, 0, len(m.Releases))}
	for _, r := range m.Releases {
		f.Versions = append(f.Versions, provider.PublishVersion{ID: r.Version, Name: r.FileName, Version: r.Version, UploadedAt: r.ReleasedAt})
		f.LastUploadedAt = r.ReleasedAt
	}
	return provider.PublishTargets{ModUID: m.Name, ModName: m.Title, FilesURL: downloadsURL(m.Name), Files: []provider.PublishFile{f}}, nil
}

// modInfo is the part of the archive's info.json that must match the request.
type modInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// readInfo reads info.json from the archive's top folder (or its root).
func readInfo(archive string) (modInfo, error) {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return modInfo{}, fmt.Errorf("not a zip archive: %w", err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		dir, base := path.Split(f.Name)
		if base != "info.json" || strings.Count(strings.Trim(dir, "/"), "/") > 0 {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return modInfo{}, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		_ = rc.Close()
		if err != nil {
			return modInfo{}, err
		}
		var mi modInfo
		if err := json.Unmarshal(b, &mi); err != nil {
			return modInfo{}, fmt.Errorf("info.json: %w", err)
		}
		return mi, nil
	}
	return modInfo{}, errors.New("no info.json in the archive's top folder")
}

// CheckPublish implements provider.Publisher (no network): a new release of
// the project's mod from a local zip whose info.json names the mod and the
// version.
func (p *Provider) CheckPublish(project provider.Project, req provider.PublishRequest) error {
	mod := project.ExternalID
	switch {
	case mod == "":
		return badPublish("empty project id")
	case req.NewFile:
		return badPublish("newFile: a Factorio mod has one file; publish a new release of it")
	case req.FileID != "" && req.FileID != mod:
		return badPublish("fileId: the mod's only file is %q", mod)
	case req.UploadID != "":
		return badPublish("uploadId: the Factorio upload API has no reusable uploads; publish from path")
	case req.Path == "":
		return badPublish("path: pick the archive to upload")
	case !filepath.IsAbs(req.Path):
		return badPublish("path must be absolute")
	case !strings.EqualFold(filepath.Ext(req.Path), ".zip"):
		return badPublish("path: a .zip archive")
	case !validVersion(req.Version):
		return badPublish("version: major.minor.patch, each 0-65535")
	case req.Changelog != "":
		return badPublish("changelog: the portal reads changelog.txt from the archive")
	}
	st, err := os.Stat(req.Path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return badPublish("path: %s is not a non-empty file", filepath.Base(req.Path))
	}
	mi, err := readInfo(req.Path)
	if err != nil {
		return badPublish("path: %v", err)
	}
	if mi.Name != mod {
		return badPublish("info.json name %q is not the mod %q", mi.Name, mod)
	}
	if mi.Version != req.Version {
		return badPublish("info.json version %q is not %q", mi.Version, req.Version)
	}
	return nil
}

// apiKey returns the stored key or, when none is stored, creates one with the
// signed-in factorio.com session (created=true).
func (p *Provider) apiKey(ctx context.Context) (string, bool, error) {
	if p.opts.Keys == nil {
		return "", false, fmt.Errorf("%w: publishing is not set up", ErrNoAPIKey)
	}
	key, err := p.opts.Keys.Key()
	if !errors.Is(err, ErrNoAPIKey) {
		return key, false, err
	}
	if _, err := p.opts.Keys.Create(ctx, p.jar()); err != nil {
		return "", false, err
	}
	key, err = p.opts.Keys.Key()
	return key, true, err
}

// apiAnswer is an upload API answer.
type apiAnswer struct {
	UploadURL string `json:"upload_url"`
	Success   bool   `json:"success"`
	Error     string `json:"error"`
	Message   string `json:"message"`
}

func apiErr(call string, status int, a apiAnswer, body []byte) error {
	if a.Error == "InvalidApiKey" {
		return fmt.Errorf("%w: %s: %s", ErrBadAPIKey, call, a.Message)
	}
	if a.Error != "" {
		return fmt.Errorf("factorio: %s: %s: %s", call, a.Error, a.Message)
	}
	return fmt.Errorf("factorio: %s: HTTP %d: %s", call, status, snippet(body))
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// send posts a multipart body with the key and reads the JSON answer.
func (p *Provider) send(ctx context.Context, call, rawURL, key, contentType string, body io.Reader, size int64) (apiAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, body)
	if err != nil {
		return apiAnswer{}, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	hc := p.opts.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		if key != "" && strings.Contains(err.Error(), key) {
			err = errors.New(strings.ReplaceAll(err.Error(), key, "***"))
		}
		return apiAnswer{}, fmt.Errorf("factorio: %s: %w", call, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var a apiAnswer
	_ = json.Unmarshal(b, &a)
	if res.StatusCode != http.StatusOK || a.Error != "" {
		return a, apiErr(call, res.StatusCode, a, b)
	}
	return a, nil
}

// initUpload asks for the upload URL of a new release of mod.
func (p *Provider) initUpload(ctx context.Context, key, mod string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("mod", mod); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	a, err := p.send(ctx, "init_upload", p.opts.Site+initUploadPath, key, mw.FormDataContentType(), &buf, int64(buf.Len()))
	if err != nil {
		return "", err
	}
	u, err := url.Parse(a.UploadURL)
	site, _ := url.Parse(p.opts.Site)
	if err != nil || u.Scheme != site.Scheme || u.Host == "" {
		return "", fmt.Errorf("factorio: init_upload: unexpected upload_url")
	}
	return a.UploadURL, nil
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

// uploadFile posts the archive as multipart field "file" (Content-Length set).
func (p *Provider) uploadFile(ctx context.Context, uploadURL, archive string, size int64, tick func(int64)) error {
	f, err := os.Open(archive) //nolint:gosec // G304: the archive the user picked
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(archive)))
	h.Set("Content-Type", "application/zip")
	if _, err := mw.CreatePart(h); err != nil {
		return err
	}
	ct := mw.FormDataContentType()
	tail := "\r\n--" + mw.Boundary() + "--\r\n"
	body := io.MultiReader(&head, &countingReader{r: io.LimitReader(f, size), tick: tick}, strings.NewReader(tail))
	a, err := p.send(ctx, "upload", uploadURL, "", ct, body, int64(head.Len())+size+int64(len(tail)))
	if err != nil {
		return err
	}
	if !a.Success {
		return errors.New("factorio: upload: the portal did not report success")
	}
	return nil
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

// Publish implements provider.Publisher. The upload POST publishes the
// release: it is sent at most once and never cancelled once sent. A release
// of the same version already on the portal is refused before anything is sent.
func (p *Provider) Publish(ctx context.Context, project provider.Project, req provider.PublishRequest, progress func(provider.PublishProgress)) (provider.PublishResult, error) {
	if progress == nil {
		progress = func(provider.PublishProgress) {}
	}
	if err := p.CheckPublish(project, req); err != nil {
		return provider.PublishResult{}, err
	}
	mod := project.ExternalID
	res := provider.PublishResult{DryRun: req.DryRun, FileID: mod, FilesURL: downloadsURL(mod)}
	fail := func(stage string, err error) (provider.PublishResult, error) {
		return res, &provider.PublishError{Stage: stage, Err: err}
	}

	progress(provider.PublishProgress{Stage: provider.StageResolve})
	m, err := p.portalMod(ctx, mod)
	if err != nil {
		return fail(provider.StageResolve, err)
	}
	if m.has(req.Version) {
		return res, badPublish("version %s of %s is already on the portal", req.Version, mod)
	}
	progress(provider.PublishProgress{Stage: provider.StageHash})
	if res.MD5, res.Size, err = fileMD5(req.Path); err != nil {
		return fail(provider.StageHash, err)
	}
	res.FileName = filepath.Base(req.Path)

	if req.DryRun {
		res.Plan = p.plan(mod, req, res)
		return res, nil
	}

	key, _, err := p.apiKey(ctx)
	if err != nil {
		return fail(provider.StageResolve, err)
	}
	uploadURL, err := p.initUpload(ctx, key, mod)
	if err != nil {
		return fail(provider.StageUpload, err)
	}
	progress(provider.PublishProgress{Stage: provider.StagePublish, Total: res.Size})
	if err := p.uploadFile(context.WithoutCancel(ctx), uploadURL, req.Path, res.Size, func(n int64) {
		progress(provider.PublishProgress{Stage: provider.StagePublish, Sent: n, Total: res.Size})
	}); err != nil {
		return fail(provider.StagePublish, err)
	}
	res.VersionID = req.Version
	return res, nil
}

// plan lists the requests a publish of req would send.
func (p *Provider) plan(mod string, req provider.PublishRequest, res provider.PublishResult) []provider.PublishStep {
	var steps []provider.PublishStep
	auth := "Authorization: Bearer <stored API key>"
	if p.opts.Keys == nil {
		auth = "no key store: publishing is not set up"
	} else if st, err := p.opts.Keys.Status(); err != nil || !st.HasAPIKey {
		form := p.opts.Keys.opts.Site + createPath
		steps = append(steps,
			provider.PublishStep{Method: http.MethodGet, URL: form, Note: "signed-in factorio.com session: the key form"},
			provider.PublishStep{Method: http.MethodPost, URL: form, Body: map[string]string{"usages": UploadUsage, "note": keyNote, "csrf_token": "{csrf}"},
				Note: "creates the API key once and stores it (data\\secrets\\" + apiFile + ")"})
		auth = "Authorization: Bearer <created API key>"
	}
	return append(steps,
		provider.PublishStep{Method: http.MethodPost, URL: p.opts.Site + initUploadPath, Body: map[string]string{"mod": mod},
			Note: "multipart/form-data; " + auth},
		provider.PublishStep{Method: http.MethodPost, URL: "{upload_url}", Body: map[string]string{"file": filepath.Base(req.Path)},
			Note: fmt.Sprintf("multipart/form-data, %d bytes, md5 %s; publishes release %s; sent once", res.Size, res.MD5, req.Version)},
	)
}
