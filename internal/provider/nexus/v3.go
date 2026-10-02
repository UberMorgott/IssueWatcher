package nexus

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: md5 is the content digest Nexus/S3 ask for, not a security hash
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// v3 is the client of the official upload API (https://api.nexusmods.com/v3,
// openapi.yaml; ported from nexusmods-mcp-server src/tools/upload-api.ts):
//
//	POST /uploads/multipart → PUT each part_presigned_url (ETag) → POST complete_presigned_url (XML)
//	→ POST /uploads/{id}/finalise → GET /uploads/{id} until state=available
//	→ POST /mod-files/{id}/versions (new version) | POST /mod-files (new file)
//	→ optional POST /mods/{uid}/changelogs
//
// Writes are never retried (a lost answer would publish twice); GETs back
// off once on 429/503; a part PUT (idempotent: same bytes, same URL) is
// retried. The API key goes to the API host only, never to the presigned
// storage URLs, and never into an error.
type v3 struct {
	opts V3Options
}

// V3Options configures the v3 client.
type V3Options struct {
	Base    string       // default https://api.nexusmods.com/v3
	HTTP    *http.Client // API and storage (default http.DefaultClient)
	Key     func() (string, error)
	Version string // Application-Version
	// PartConcurrency is how many parts upload at once (default 4, as the MCP).
	PartConcurrency int
	// PollWait is the pause before poll attempt n (default 2 s × 1.5ⁿ, at most 30 s);
	// PollTimeout bounds the wait for state=available (default 15 min).
	PollWait    func(n int) time.Duration
	PollTimeout time.Duration
}

func newV3(o V3Options) *v3 {
	if o.Base == "" {
		o.Base = apiV3
	}
	o.Base = strings.TrimRight(o.Base, "/")
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	if o.Key == nil {
		o.Key = func() (string, error) { return "", ErrNoAPIKey }
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.PartConcurrency <= 0 {
		o.PartConcurrency = 4
	}
	if o.PollWait == nil {
		o.PollWait = func(n int) time.Duration {
			d := 2 * time.Second
			for range n {
				d = d * 3 / 2
			}
			return min(d, 30*time.Second)
		}
	}
	if o.PollTimeout <= 0 {
		o.PollTimeout = 15 * time.Minute
	}
	return &v3{opts: o}
}

// V3Error is a refused v3 request (RFC 9457 problem details when given).
type V3Error struct {
	Status int
	Method string
	Path   string // without query
	Title  string
	Detail string
}

func (e *V3Error) Error() string {
	msg := strings.TrimSpace(cmpName(e.Detail, e.Title))
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("nexus v3: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, msg)
}

// Unwrap maps 401 to ErrBadAPIKey.
func (e *V3Error) Unwrap() error {
	if e.Status == http.StatusUnauthorized {
		return ErrBadAPIKey
	}
	return nil
}

// UploadError: the bytes were uploaded (and finalised) but the upload did not
// become available in time; UploadID can be published later without
// re-uploading.
type UploadError struct {
	UploadID string
	Err      error
}

func (e *UploadError) Error() string {
	return fmt.Sprintf("nexus v3: upload %s: %v", e.UploadID, e.Err)
}

func (e *UploadError) Unwrap() error { return e.Err }

// errNotAvailable: polling ended before state=available.
var errNotAvailable = errors.New("not available yet; publish later with this upload id")

// ── JSON calls ────────────────────────────────────────────────────

// call sends a JSON request to the API; out receives the "data" member.
func (c *v3) call(ctx context.Context, method, path string, body, out any) error {
	key, err := c.opts.Key()
	if err != nil {
		return err
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.opts.Base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		setAPIHeaders(req.Header, key, c.opts.Version)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := c.opts.HTTP.Do(req)
		if err != nil {
			return fmt.Errorf("nexus v3: %s %s: %w", method, path, redactErr(err, key))
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		_ = res.Body.Close()
		if method == http.MethodGet && attempt == 0 && (res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable) {
			if err := sleepCtx(ctx, retryAfter(res.Header.Get("Retry-After"))); err != nil {
				return err
			}
			continue
		}
		if res.StatusCode < 200 || res.StatusCode > 299 {
			e := &V3Error{Status: res.StatusCode, Method: method, Path: path}
			var p struct {
				Title  string `json:"title"`
				Detail string `json:"detail"`
			}
			if json.Unmarshal(b, &p) == nil && (p.Title != "" || p.Detail != "") {
				e.Title, e.Detail = p.Title, p.Detail
			} else {
				e.Detail = snippet(b)
			}
			e.Title, e.Detail = redact(e.Title, key), redact(e.Detail, key)
			return e
		}
		if out == nil {
			return nil
		}
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(b, &env); err != nil || len(env.Data) == 0 {
			return fmt.Errorf("nexus v3: %s %s: unexpected answer: %s", method, path, redact(snippet(b), key))
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("nexus v3: %s %s: %w", method, path, err)
		}
		return nil
	}
}

func retryAfter(h string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 {
		return min(time.Duration(n)*time.Second, 30*time.Second)
	}
	return 2 * time.Second
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ── reads ─────────────────────────────────────────────────────────

// V3Mod is GET /games/{game}/mods/{id} (schema Mod).
type V3Mod struct {
	ID           string  `json:"id"` // the mod uid other v3 calls take
	GameScopedID string  `json:"game_scoped_id"`
	GameID       string  `json:"game_id"`
	Name         *string `json:"name"`
}

// V3ModFile is one mod file (schema ModFileWithAggregates).
type V3ModFile struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	IsActive           bool    `json:"is_active"`
	LastFileUploadedAt *string `json:"last_file_uploaded_at"`
	VersionsCount      int     `json:"versions_count"`
	ArchivedCount      int     `json:"archived_count"`
	RemovedCount       int     `json:"removed_count"`
}

// V3Version is one version of a mod file (schema ModFileVersion).
type V3Version struct {
	ID   string `json:"id"`
	File struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"file"`
	Position     string `json:"position"`
	GameScopedID string `json:"game_scoped_id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Category     string `json:"category"`
	UploadedAt   string `json:"uploaded_at"`
	IsPrimary    bool   `json:"is_primary"`
}

func (c *v3) mod(ctx context.Context, game string, modID int) (V3Mod, error) {
	var m V3Mod
	err := c.call(ctx, http.MethodGet, "/games/"+url.PathEscape(game)+"/mods/"+strconv.Itoa(modID), nil, &m)
	return m, err
}

func (c *v3) modFiles(ctx context.Context, modUID string) ([]V3ModFile, error) {
	var r struct {
		ModFiles []V3ModFile `json:"mod_files"`
	}
	err := c.call(ctx, http.MethodGet, "/mods/"+url.PathEscape(modUID)+"/files", nil, &r)
	return r.ModFiles, err
}

func (c *v3) fileVersions(ctx context.Context, fileID string) ([]V3Version, error) {
	var r struct {
		Versions []V3Version `json:"versions"`
	}
	err := c.call(ctx, http.MethodGet, "/mod-files/"+url.PathEscape(fileID)+"/versions", nil, &r)
	return r.Versions, err
}

// ── writes ────────────────────────────────────────────────────────

// v3VersionBody is CreateModFileVersionRequest (POST /mod-files/{id}/versions).
type v3VersionBody struct {
	UploadID                  string  `json:"upload_id"`
	Name                      string  `json:"name"`
	Description               *string `json:"description,omitempty"`
	Version                   string  `json:"version"`
	FileCategory              string  `json:"file_category"`
	PrimaryModManagerDownload *bool   `json:"primary_mod_manager_download,omitempty"`
	AllowModManagerDownload   *bool   `json:"allow_mod_manager_download,omitempty"`
	ShowRequirementsPopUp     *bool   `json:"show_requirements_pop_up,omitempty"`
	UpdateModVersion          bool    `json:"update_mod_version"`
	ArchiveExistingFile       bool    `json:"archive_existing_file"`
	PreviousVersionID         *string `json:"previous_version_id,omitempty"`
}

// v3FileBody is CreateModFileRequest (POST /mod-files): a new file, no
// archive / previous-version fields.
type v3FileBody struct {
	UploadID                  string  `json:"upload_id"`
	ModID                     string  `json:"mod_id"` // the mod uid
	Name                      string  `json:"name"`
	Version                   string  `json:"version"`
	Description               *string `json:"description,omitempty"`
	FileCategory              string  `json:"file_category"`
	PrimaryModManagerDownload *bool   `json:"primary_mod_manager_download,omitempty"`
	AllowModManagerDownload   *bool   `json:"allow_mod_manager_download,omitempty"`
	ShowRequirementsPopUp     *bool   `json:"show_requirements_pop_up,omitempty"`
	UpdateModVersion          bool    `json:"update_mod_version"`
}

// V3UploadModFile is the mod file a publish created or extended (schema UploadModFile).
type V3UploadModFile struct {
	ID           string `json:"id"`
	GameScopedID string `json:"game_scoped_id"`
	Name         string `json:"name"`
	FileCategory string `json:"file_category"`
}

// v3VersionResult is CreateModFileVersionSuccess.
type v3VersionResult struct {
	File    V3UploadModFile `json:"file"`
	Version struct {
		ID       string `json:"id"`
		Position string `json:"position"`
	} `json:"version"`
}

func (c *v3) createVersion(ctx context.Context, fileID string, b v3VersionBody) (v3VersionResult, error) {
	var r v3VersionResult
	err := c.call(ctx, http.MethodPost, "/mod-files/"+url.PathEscape(fileID)+"/versions", b, &r)
	return r, err
}

func (c *v3) createFile(ctx context.Context, b v3FileBody) (V3UploadModFile, error) {
	var r V3UploadModFile
	err := c.call(ctx, http.MethodPost, "/mod-files", b, &r)
	return r, err
}

func (c *v3) addChangelog(ctx context.Context, modUID, version, text string) error {
	return c.call(ctx, http.MethodPost, "/mods/"+url.PathEscape(modUID)+"/changelogs",
		map[string]string{"version": version, "changelog": text}, nil)
}

// ── upload ────────────────────────────────────────────────────────

// v3Multipart is CreateMultipartUploadSuccess.
type v3Multipart struct {
	ID       string   `json:"id"`
	State    string   `json:"state"`
	PartSize int64    `json:"part_size_bytes"`
	PartURLs []string `json:"part_presigned_urls"`
	Complete string   `json:"complete_presigned_url"`
}

// upload sends the archive at path and returns the upload id once its state
// is available. progress (may be nil) gets the bytes sent so far; waiting
// (may be nil) is called once the parts are in, before finalise and the poll.
// CreateUploadRequest (multipart) has no md5 field (only the single-part
// CreateSinglePartUploadRequest has one), so none is sent here.
func (c *v3) upload(ctx context.Context, path string, progress func(sent, total int64), waiting func()) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the archive the user picked to publish
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()
	if size <= 0 || !st.Mode().IsRegular() {
		return "", fmt.Errorf("nexus v3: %s is empty or not a file", filepath.Base(path))
	}
	var mp v3Multipart
	if err := c.call(ctx, http.MethodPost, "/uploads/multipart",
		map[string]any{"filename": filepath.Base(path), "size_bytes": size}, &mp); err != nil {
		return "", err
	}
	if mp.ID == "" || mp.PartSize <= 0 || mp.Complete == "" || int64(len(mp.PartURLs)) != (size+mp.PartSize-1)/mp.PartSize {
		return "", fmt.Errorf("nexus v3: upload %s: %d part URLs of %d bytes do not cover %d bytes", mp.ID, len(mp.PartURLs), mp.PartSize, size)
	}
	etags, err := c.putParts(ctx, f, size, mp, progress)
	if err != nil {
		return "", fmt.Errorf("nexus v3: upload %s: %w", mp.ID, err)
	}
	if err := c.complete(ctx, mp.Complete, etags); err != nil {
		return "", fmt.Errorf("nexus v3: upload %s: %w", mp.ID, err)
	}
	if waiting != nil {
		waiting()
	}
	if err := c.call(ctx, http.MethodPost, "/uploads/"+url.PathEscape(mp.ID)+"/finalise", nil, nil); err != nil {
		return "", err
	}
	return mp.ID, c.waitAvailable(ctx, mp.ID)
}

// putParts uploads the parts, PartConcurrency at a time, and returns their
// ETags (quotes removed) in part order.
func (c *v3) putParts(ctx context.Context, f io.ReaderAt, size int64, mp v3Multipart, progress func(sent, total int64)) ([]string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	etags := make([]string, len(mp.PartURLs))
	var sent atomic.Int64
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	sem := make(chan struct{}, c.opts.PartConcurrency)
	for i, u := range mp.PartURLs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			off := int64(i) * mp.PartSize
			n := min(mp.PartSize, size-off)
			buf := make([]byte, n)
			if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
				once.Do(func() { first = err; cancel() })
				return
			}
			etag, err := c.putPart(ctx, u, buf)
			if err != nil {
				once.Do(func() { first = fmt.Errorf("part %d: %w", i+1, err); cancel() })
				return
			}
			etags[i] = etag
			if progress != nil {
				progress(sent.Add(n), size)
			}
		})
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return etags, nil
}

// putPart PUTs one part to its presigned URL (no API key) and returns its
// ETag; transport errors and 5xx are retried twice (same bytes, same URL).
func (c *v3) putPart(ctx context.Context, u string, data []byte) (string, error) {
	var last error
	for attempt := range 3 {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt)*time.Second); err != nil {
				return "", err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data))
		if err != nil {
			return "", err
		}
		req.ContentLength = int64(len(data))
		req.Header.Set("Content-Type", "application/octet-stream")
		res, err := c.opts.HTTP.Do(req)
		if err != nil {
			last = fmt.Errorf("storage PUT: %w", stripQueryErr(err))
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		_ = res.Body.Close()
		switch {
		case res.StatusCode >= 500:
			last = fmt.Errorf("storage PUT: HTTP %d: %s", res.StatusCode, snippet(b))
			continue
		case res.StatusCode < 200 || res.StatusCode > 299:
			return "", fmt.Errorf("storage PUT: HTTP %d: %s", res.StatusCode, snippet(b))
		}
		etag := strings.Trim(strings.TrimSpace(res.Header.Get("ETag")), `"`)
		if etag == "" {
			return "", errors.New("storage PUT: no ETag")
		}
		return etag, nil
	}
	return "", last
}

// completeXML is the S3 CompleteMultipartUpload body: parts in order, ETags unquoted.
func completeXML(etags []string) []byte {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>\n")
	for i, e := range etags {
		fmt.Fprintf(&b, "  <Part>\n    <PartNumber>%d</PartNumber>\n    <ETag>", i+1)
		_ = xml.EscapeText(&b, []byte(e))
		b.WriteString("</ETag>\n  </Part>\n")
	}
	b.WriteString("</CompleteMultipartUpload>")
	return []byte(b.String())
}

// complete POSTs the part list to the presigned complete URL. S3 may answer
// 200 with an <Error> body: that is a failure too.
func (c *v3) complete(ctx context.Context, u string, etags []string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(completeXML(etags)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	res, err := c.opts.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("storage complete: %w", stripQueryErr(err))
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 || bytes.Contains(b, []byte("<Error>")) {
		return fmt.Errorf("storage complete: HTTP %d: %s", res.StatusCode, snippet(b))
	}
	return nil
}

// waitAvailable polls GET /uploads/{id} until state=available.
func (c *v3) waitAvailable(ctx context.Context, id string) error {
	deadline := time.Now().Add(c.opts.PollTimeout)
	for n := 0; ; n++ {
		var up struct {
			State string `json:"state"`
		}
		if err := c.call(ctx, http.MethodGet, "/uploads/"+url.PathEscape(id), nil, &up); err != nil {
			return &UploadError{UploadID: id, Err: err}
		}
		if up.State == "available" {
			return nil
		}
		wait := c.opts.PollWait(n)
		if time.Now().Add(wait).After(deadline) {
			return &UploadError{UploadID: id, Err: errNotAvailable}
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return &UploadError{UploadID: id, Err: err}
		}
	}
}

// stripQueryErr drops a presigned URL's query (its signature) from err's text.
func stripQueryErr(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		if p, perr := url.Parse(ue.URL); perr == nil {
			p.RawQuery = ""
			return &url.Error{Op: ue.Op, URL: p.String(), Err: ue.Err}
		}
	}
	return err
}

// fileMD5 is the hex MD5 of the file at path (shown in a plan and a result:
// the digest single-part uploads will need from 2026-12-01).
func fileMD5(path string) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the archive the user picked to publish
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := md5.New() //nolint:gosec // G401: content digest, not security
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
