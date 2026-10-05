package steamcmd

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

const userAgent = "IssueWatcher (+https://github.com/UberMorgott/issuewatcher)"

// itemDetails is the part of ISteamRemoteStorage/GetPublishedFileDetails read here.
type itemDetails struct {
	PublishedFileID string `json:"publishedfileid"`
	Result          int    `json:"result"`
	Creator         string `json:"creator"`
	ConsumerAppID   int    `json:"consumer_app_id"`
	Title           string `json:"title"`
	TimeUpdated     int64  `json:"time_updated"`
	Visibility      int    `json:"visibility"` // 0 public, 1 friends, 2 private, 3 unlisted
	Banned          int    `json:"banned"`
}

func (w *Workshop) get(ctx context.Context, method, u string, form url.Values) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := w.opts.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("steam: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("steam: %s: HTTP %d", u, res.StatusCode)
	}
	return b, nil
}

// details reads the item (public, keyless).
func (w *Workshop) details(ctx context.Context, id string) (itemDetails, error) {
	b, err := w.get(ctx, http.MethodPost, w.opts.APIURL+"/ISteamRemoteStorage/GetPublishedFileDetails/v1/",
		url.Values{"itemcount": {"1"}, "publishedfileids[0]": {id}})
	if err != nil {
		return itemDetails{}, err
	}
	var a struct {
		Response struct {
			Details []itemDetails `json:"publishedfiledetails"`
		} `json:"response"`
	}
	if err := json.Unmarshal(b, &a); err != nil || len(a.Response.Details) != 1 {
		return itemDetails{}, fmt.Errorf("steam: item %s: unexpected answer", id)
	}
	d := a.Response.Details[0]
	if d.Result != 1 || d.PublishedFileID != id {
		return itemDetails{}, fmt.Errorf("steam: item %s not found (result %d)", id, d.Result)
	}
	return d, nil
}

// changeNote is one entry of the item's change notes page.
type changeNote struct {
	At   int64 // unix time = the update's time_updated
	Text string
}

var (
	noteRe    = regexp.MustCompile(`(?s)<p id="(\d+)">(.*?)</p>`)
	tagRe     = regexp.MustCompile(`<[^>]*>`)
	semverRe  = regexp.MustCompile(`\bv?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)\b`)
	itemIDRe  = regexp.MustCompile(`^\d{1,20}$`)
	vdfUnsafe = strings.NewReplacer(`"`, `'`, "\x00", "")
)

// changeNotes reads the first page of the item's change notes, newest first.
func (w *Workshop) changeNotes(ctx context.Context, id string) ([]changeNote, error) {
	b, err := w.get(ctx, http.MethodGet, w.opts.Site+"/sharedfiles/filedetails/changelog/"+id, nil)
	if err != nil {
		return nil, err
	}
	return parseChangeNotes(string(b)), nil
}

func parseChangeNotes(page string) []changeNote {
	var out []changeNote
	for _, m := range noteRe.FindAllStringSubmatch(page, -1) {
		at, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, changeNote{At: at, Text: strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(m[2], " ")))})
	}
	return out
}

// noteVersion is the first version number in a change note ("" = none).
func noteVersion(text string) string {
	if m := semverRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

func itemURL(id string) string { return "https://steamcommunity.com/sharedfiles/filedetails/?id=" + id }

// ready: steamcmd is there and signed in (as far as known without running it).
func (w *Workshop) ready() error {
	s, err := w.user()
	if err != nil {
		return err
	}
	if s.Expired {
		return ErrRelogin
	}
	_, _, err = w.steamcmd()
	return err
}

// PublishTargets implements provider.Publisher: the item is one file whose
// versions are the change notes naming a version (oldest first). A version
// is Pending while the item is private or banned. Without a steamcmd sign-in
// it reports the auth error (the plan shows it).
func (w *Workshop) PublishTargets(ctx context.Context, project provider.Project) (provider.PublishTargets, error) {
	id := project.ExternalID
	if !itemIDRe.MatchString(id) {
		return provider.PublishTargets{}, fmt.Errorf("steam: %q is not a Workshop item id", id)
	}
	if err := w.ready(); err != nil {
		return provider.PublishTargets{}, err
	}
	d, err := w.details(ctx, id)
	if err != nil {
		return provider.PublishTargets{}, err
	}
	notes, err := w.changeNotes(ctx, id)
	if err != nil {
		return provider.PublishTargets{}, err
	}
	pending := d.Visibility == 2 || d.Banned != 0
	f := provider.PublishFile{ID: id, Name: d.Title, Active: true}
	for _, n := range slices.Backward(notes) {
		v := noteVersion(n.Text)
		if v == "" {
			continue
		}
		at := time.Unix(n.At, 0).UTC().Format(time.RFC3339)
		f.Versions = append(f.Versions, provider.PublishVersion{ID: strconv.FormatInt(n.At, 10), Name: d.Title, Version: v, UploadedAt: at,
			Pending: pending || n.At > d.TimeUpdated})
		f.LastUploadedAt = at
	}
	f.VersionsCount = len(f.Versions)
	return provider.PublishTargets{ModUID: id, ModName: d.Title, FilesURL: w.opts.Site + "/sharedfiles/filedetails/changelog/" + id,
		Files: []provider.PublishFile{f}}, nil
}

func badPublish(format string, a ...any) error {
	return fmt.Errorf("%w: %s", provider.ErrBadPublish, fmt.Sprintf(format, a...))
}

// CheckPublish implements provider.Publisher (no network): an update of the
// item from a content folder or a zip of it.
func (w *Workshop) CheckPublish(project provider.Project, req provider.PublishRequest) error {
	switch {
	case !itemIDRe.MatchString(project.ExternalID):
		return badPublish("%q is not a Workshop item id", project.ExternalID)
	case req.NewFile:
		return badPublish("newFile: create the Workshop item in the game first; the app updates existing items")
	case req.FileID != "" && req.FileID != project.ExternalID:
		return badPublish("fileId: the item's only target is %s", project.ExternalID)
	case req.UploadID != "":
		return badPublish("uploadId: steamcmd has no reusable uploads")
	case req.Path == "":
		return badPublish("path: the content folder or a zip of it")
	case !filepath.IsAbs(req.Path):
		return badPublish("path must be absolute")
	case semverRe.FindString(req.Version) == "":
		return badPublish("version: like 1.2.3")
	case req.AppID < 0:
		return badPublish("appId")
	case len([]rune(req.Changelog)) > 8000:
		return badPublish("changelog: at most 8000 characters")
	}
	st, err := os.Stat(req.Path)
	switch {
	case err != nil:
		return badPublish("path: %s not found", filepath.Base(req.Path))
	case st.IsDir():
		return nil
	case !st.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(req.Path), ".zip"):
		return badPublish("path: a folder or a .zip")
	}
	return nil
}

// changeNote is the item's change note for req: it names the version.
func changeNoteFor(req provider.PublishRequest) string {
	note := strings.TrimSpace(req.Changelog)
	v := "v" + strings.TrimPrefix(req.Version, "v")
	if !strings.Contains(note, strings.TrimPrefix(req.Version, "v")) {
		if note == "" {
			note = v
		} else {
			note = v + " - " + note
		}
	}
	return vdfUnsafe.Replace(note)
}

// vdf is the workshop_build_item file. Paths use forward slashes (no escape
// sequences); quotes in the note become apostrophes.
func vdf(appID int, id, content, note string) string {
	return fmt.Sprintf("\"workshopitem\"\n{\n\t\"appid\"\t\t\"%d\"\n\t\"publishedfileid\"\t\t\"%s\"\n\t\"contentfolder\"\t\t\"%s\"\n\t\"changenote\"\t\t\"%s\"\n}\n",
		appID, id, filepath.ToSlash(content), note)
}

// manifest is the content folder's sorted relative paths with sha256: its
// digest identifies the uploaded content.
type manifest struct {
	Files  int
	Size   int64
	Digest string // sha256 of "path\tsha256\n" lines
}

func contentManifest(dir string) (manifest, error) {
	type entry struct{ rel, sum string }
	var es []entry
	var m manifest
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		f, err := os.Open(p) //nolint:gosec // G304: the content folder
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		es = append(es, entry{filepath.ToSlash(rel), hex.EncodeToString(h.Sum(nil))})
		m.Size += n
		return nil
	})
	if err != nil {
		return manifest{}, err
	}
	if len(es) == 0 {
		return manifest{}, errors.New("the content folder is empty")
	}
	slices.SortFunc(es, func(a, b entry) int { return strings.Compare(a.rel, b.rel) })
	h := sha256.New()
	for _, e := range es {
		_, _ = fmt.Fprintf(h, "%s\t%s\n", e.rel, e.sum)
	}
	m.Files, m.Digest = len(es), hex.EncodeToString(h.Sum(nil))
	return m, nil
}

// unpack extracts zip into dir (emptied first), refusing paths that leave it.
func unpack(zipPath, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("not a zip archive: %w", err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		name := filepath.FromSlash(f.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%s leaves the archive", f.Name)
		}
		dst := filepath.Join(dir, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: inside the app's content dir
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(rc, 4<<30))
		_ = rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// content is the folder steamcmd uploads: req.Path itself, or the zip
// unpacked into data\tools\steamcmd\content\<item>.
func (w *Workshop) content(id, path string) (string, error) {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		return path, nil
	}
	dir := filepath.Join(w.opts.DataDir, "tools", "steamcmd", "content", id)
	if err := unpack(path, dir); err != nil {
		return "", badPublish("path: %v", err)
	}
	return dir, nil
}

// Publish implements provider.Publisher: one workshop_build_item run, sent
// at most once. An expired sign-in is ErrRelogin (nothing was uploaded).
func (w *Workshop) Publish(ctx context.Context, project provider.Project, req provider.PublishRequest, progress func(provider.PublishProgress)) (provider.PublishResult, error) {
	if progress == nil {
		progress = func(provider.PublishProgress) {}
	}
	if err := w.CheckPublish(project, req); err != nil {
		return provider.PublishResult{}, err
	}
	id := project.ExternalID
	res := provider.PublishResult{DryRun: req.DryRun, FileID: id, FilesURL: w.opts.Site + "/sharedfiles/filedetails/changelog/" + id}
	fail := func(stage string, err error) (provider.PublishResult, error) {
		return res, &provider.PublishError{Stage: stage, Err: err}
	}

	progress(provider.PublishProgress{Stage: provider.StageResolve})
	d, err := w.details(ctx, id)
	if err != nil {
		return fail(provider.StageResolve, err)
	}
	appID := req.AppID
	if appID == 0 {
		appID = d.ConsumerAppID
	} else if d.ConsumerAppID != 0 && appID != d.ConsumerAppID {
		return res, badPublish("appId %d: item %s belongs to app %d", appID, id, d.ConsumerAppID)
	}
	pt, err := w.PublishTargets(ctx, project)
	if err != nil {
		return fail(provider.StageResolve, err)
	}
	for _, v := range pt.Files[0].Versions {
		if strings.TrimPrefix(v.Version, "v") == strings.TrimPrefix(req.Version, "v") {
			return res, badPublish("version %s of item %s is already in its change notes", req.Version, id)
		}
	}

	progress(provider.PublishProgress{Stage: provider.StageHash})
	dir, err := w.content(id, req.Path)
	if err != nil {
		return res, err
	}
	m, err := contentManifest(dir)
	if err != nil {
		return res, badPublish("content: %v", err)
	}
	res.Size, res.MD5, res.FileName = m.Size, "", fmt.Sprintf("%d files, content sha256 %s", m.Files, m.Digest)
	note := changeNoteFor(req)
	vdfPath := filepath.Join(w.opts.DataDir, "tools", "steamcmd", "builds", id+".vdf")

	s, uerr := w.user()
	exe, _, xerr := w.steamcmd()
	if req.DryRun {
		res.Plan = w.plan(id, appID, dir, note, m, s.User, errors.Join(uerr, xerr))
		return res, nil
	}
	if uerr != nil {
		return fail(provider.StageResolve, uerr)
	}
	if s.Expired {
		return fail(provider.StageResolve, ErrRelogin)
	}
	if xerr != nil {
		return fail(provider.StageResolve, xerr)
	}
	if err := os.MkdirAll(filepath.Dir(vdfPath), 0o700); err != nil {
		return fail(provider.StageUpload, err)
	}
	if err := os.WriteFile(vdfPath, []byte(vdf(appID, id, dir, note)), 0o600); err != nil {
		return fail(provider.StageUpload, err)
	}
	if err := w.lock(ctx, 2*time.Minute); err != nil {
		return fail(provider.StageResolve, err)
	}
	progress(provider.PublishProgress{Stage: provider.StagePublish, Total: m.Size})
	b, err := w.runBatch(context.WithoutCancel(ctx), exe, batchArgs(s.User, "+workshop_build_item", vdfPath), w.opts.UploadTimeout)
	w.run.Unlock()
	switch {
	case err != nil:
		return fail(provider.StagePublish, err) // outcome unknown: the probe decides
	case b.loginFailed():
		w.markExpired()
		return fail(provider.StagePublish, ErrRelogin)
	case uploadErr.MatchString(b.out):
		return fail(provider.StagePublish, fmt.Errorf("steam: %s", scrub(lastLine(uploadErr, b.out))))
	case b.code != 0 || !successRe.MatchString(b.out):
		return fail(provider.StagePublish, fmt.Errorf("steam: steamcmd exited (code %d) without reporting success", b.code))
	}
	_, _ = w.update(func(s *settings) { s.Expired, s.CheckedAt = false, w.opts.Now().UTC() })
	res.VersionID = req.Version
	return res, nil
}

// plan lists what a publish of req would run.
func (w *Workshop) plan(id string, appID int, dir, note string, m manifest, user string, authErr error) []provider.PublishStep {
	login := "+login " + user + " (cached steamcmd sign-in)"
	if authErr != nil {
		login = "not ready: " + authErr.Error()
	}
	return []provider.PublishStep{
		{Method: "RUN", URL: "steamcmd +@ShutdownOnFailedCommand 1 +@NoPromptForPassword 1 +login <user> +workshop_build_item <vdf> +quit",
			Body: map[string]any{"appid": appID, "publishedfileid": id, "contentfolder": filepath.ToSlash(dir), "changenote": note},
			Note: fmt.Sprintf("%s; content %d files, %d bytes, sha256 %s; updates %s once", login, m.Files, m.Size, m.Digest, itemURL(id))},
	}
}
