package nexus

import (
	"crypto/md5" //nolint:gosec // G501: fake S3 ETags are MD5s, as on S3
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNexus is the v3 API plus a presigned-S3 storage, both httptest. No test
// ever reaches api.nexusmods.com.
type fakeNexus struct {
	t        *testing.T
	api, s3  *httptest.Server
	key      string
	partSize int64

	mu           sync.Mutex
	calls        []string          // "METHOD /path" of API calls, in order
	bodies       map[string][]byte // last JSON body per "METHOD /path"
	parts        map[int][]byte
	completeX    string
	s3Keys       int // storage requests that carried an apikey (must stay 0)
	badHeaders   int // API requests without the key or the Application-* headers
	pollsLeft    int // GET /uploads/{id} answers "created" this many times
	versionFail  int // status for POST /mod-files/{id}/versions (0 = 201)
	versionPOSTs int
}

const fakeUploadID = "0b3f6c55-1111-4222-8333-944445555666"

func newFakeNexus(t *testing.T) *fakeNexus {
	t.Helper()
	f := &fakeNexus{t: t, key: testKey, partSize: 1000, bodies: map[string][]byte{}, parts: map[int][]byte{}, pollsLeft: 2}
	f.s3 = httptest.NewServer(http.HandlerFunc(f.storage))
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	t.Cleanup(f.s3.Close)
	t.Cleanup(f.api.Close)
	return f
}

func (f *fakeNexus) client() *v3 {
	return newV3(V3Options{Base: f.api.URL + "/v3", HTTP: f.api.Client(), Key: func() (string, error) { return f.key, nil },
		Version: "1.2.3", PollWait: func(int) time.Duration { return time.Millisecond }, PollTimeout: 5 * time.Second})
}

func (f *fakeNexus) storage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("apikey") != "" {
		f.s3Keys++
	}
	if r.URL.Query().Get("X-Amz-Signature") != "sig" {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/part/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/part/"))
		b, _ := io.ReadAll(r.Body)
		if int64(len(b)) != r.ContentLength {
			http.Error(w, "length mismatch", http.StatusBadRequest)
			return
		}
		f.parts[n] = b
		sum := md5.Sum(b) //nolint:gosec // G401: S3 ETag
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	case r.Method == http.MethodPost && r.URL.Path == "/complete":
		b, _ := io.ReadAll(r.Body)
		f.completeX = string(b)
		_, _ = w.Write([]byte("<CompleteMultipartUploadResult></CompleteMultipartUploadResult>"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeNexus) data(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
}

func (f *fakeNexus) problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail, "instance": "/x"})
}

func (f *fakeNexus) serveAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v3")
	call := r.Method + " " + p
	f.calls = append(f.calls, call)
	if r.Header.Get("Application-Name") != AppName || r.Header.Get("Application-Version") != "1.2.3" {
		f.badHeaders++
	}
	if r.Header.Get("apikey") != testKey {
		f.badHeaders++
		f.problem(w, http.StatusUnauthorized, "Invalid API key")
		return
	}
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		f.bodies[call] = b
	}
	switch call {
	case "GET /games/wartales/mods/202":
		f.data(w, 200, map[string]any{"id": "mod-uid-202", "game_scoped_id": "202", "game_id": "g-1", "name": "Wartales MP"})
	case "GET /mods/mod-uid-202/files":
		f.data(w, 200, map[string]any{"mod_files": []map[string]any{
			{"id": "file-main", "name": "Wartales MP", "is_active": true, "last_file_uploaded_at": "2026-09-01T10:00:00Z", "versions_count": 2, "archived_count": 1, "removed_count": 0},
		}})
	case "GET /mod-files/file-main/versions":
		f.data(w, 200, map[string]any{"versions": []map[string]any{
			{"id": "ver-1", "file": map[string]any{"id": "file-main", "name": "Wartales MP"}, "position": "1", "game_scoped_id": "900", "name": "Wartales MP", "version": "0.1.0", "category": "archived", "uploaded_at": "2026-08-01T10:00:00Z"},
			{"id": "ver-2", "file": map[string]any{"id": "file-main", "name": "Wartales MP"}, "position": "2", "game_scoped_id": "901", "name": "Wartales MP", "version": "0.2.0", "category": "main", "uploaded_at": "2026-09-01T10:00:00Z", "is_primary": true},
		}})
	case "POST /uploads/multipart":
		var req struct {
			Filename  string `json:"filename"`
			SizeBytes *int64 `json:"size_bytes"` // a JSON number (format int64)
		}
		if err := json.Unmarshal(f.bodies[call], &req); err != nil || req.Filename == "" || req.SizeBytes == nil {
			f.problem(w, 422, "bad body")
			return
		}
		n := (*req.SizeBytes + f.partSize - 1) / f.partSize
		urls := make([]string, n)
		for i := range urls {
			urls[i] = fmt.Sprintf("%s/part/%d?X-Amz-Signature=sig", f.s3.URL, i+1)
		}
		f.data(w, 201, map[string]any{"id": fakeUploadID, "user": map[string]any{"id": "u1"}, "state": "created",
			"part_size_bytes": f.partSize, "part_presigned_urls": urls, "complete_presigned_url": f.s3.URL + "/complete?X-Amz-Signature=sig"})
	case "POST /uploads/" + fakeUploadID + "/finalise":
		f.data(w, 200, map[string]any{"id": fakeUploadID, "user": map[string]any{"id": "u1"}, "state": "created"})
	case "GET /uploads/" + fakeUploadID:
		state := "available"
		if f.pollsLeft > 0 {
			f.pollsLeft--
			state = "created"
		}
		f.data(w, 200, map[string]any{"id": fakeUploadID, "user": map[string]any{"id": "u1"}, "state": state})
	case "POST /mod-files/file-main/versions":
		f.versionPOSTs++
		if f.versionFail != 0 {
			f.problem(w, f.versionFail, "version already exists")
			return
		}
		f.data(w, 201, map[string]any{"file": map[string]any{"id": "file-main", "game_scoped_id": "777", "name": "Wartales MP", "file_category": "main"},
			"version": map[string]any{"id": "ver-3", "position": "3"}})
	case "POST /mod-files":
		f.data(w, 201, map[string]any{"id": "file-new", "game_scoped_id": "778", "name": "Wartales MP tools", "file_category": "optional"})
	case "POST /mods/mod-uid-202/changelogs":
		f.data(w, 201, map[string]any{"version": "0.3.0", "changelog": "x"})
	default:
		f.problem(w, 404, "no route "+call)
	}
}

func writeArchive(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/13)
	}
	p := filepath.Join(t.TempDir(), "wartales-mp-0.3.0.zip")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, data
}

// Multipart upload: exact part sizes, ETags unquoted in part order in the
// complete XML, no API key at storage, finalise, poll until available.
func TestV3UploadMultipart(t *testing.T) {
	f := newFakeNexus(t)
	path, data := writeArchive(t, 2500)
	var last int64
	var mu sync.Mutex
	id, err := f.client().upload(t.Context(), path, func(sent, total int64) {
		mu.Lock()
		last = max(last, sent)
		mu.Unlock()
		if total != 2500 {
			t.Errorf("total %d", total)
		}
	})
	if err != nil || id != fakeUploadID {
		t.Fatalf("upload: %q %v", id, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.parts) != 3 || len(f.parts[1]) != 1000 || len(f.parts[2]) != 1000 || len(f.parts[3]) != 500 {
		t.Fatalf("parts: %d (%d %d %d)", len(f.parts), len(f.parts[1]), len(f.parts[2]), len(f.parts[3]))
	}
	if got := string(f.parts[1]) + string(f.parts[2]) + string(f.parts[3]); got != string(data) {
		t.Fatal("parts do not reassemble the file")
	}
	var want strings.Builder
	want.WriteString("<CompleteMultipartUpload>\n")
	for i := 1; i <= 3; i++ {
		sum := md5.Sum(f.parts[i]) //nolint:gosec // G401: S3 ETag
		fmt.Fprintf(&want, "  <Part>\n    <PartNumber>%d</PartNumber>\n    <ETag>%s</ETag>\n  </Part>\n", i, hex.EncodeToString(sum[:]))
	}
	want.WriteString("</CompleteMultipartUpload>")
	if f.completeX != want.String() {
		t.Fatalf("complete XML:\n%s\nwant:\n%s", f.completeX, want.String())
	}
	if body := string(f.bodies["POST /uploads/multipart"]); body != `{"filename":"wartales-mp-0.3.0.zip","size_bytes":2500}` {
		t.Fatalf("multipart body %s", body)
	}
	wantCalls := []string{"POST /uploads/multipart", "POST /uploads/" + fakeUploadID + "/finalise",
		"GET /uploads/" + fakeUploadID, "GET /uploads/" + fakeUploadID, "GET /uploads/" + fakeUploadID}
	if strings.Join(f.calls, "|") != strings.Join(wantCalls, "|") {
		t.Fatalf("calls %v", f.calls)
	}
	if f.s3Keys != 0 || f.badHeaders != 0 {
		t.Fatalf("api key at storage %d, bad API headers %d", f.s3Keys, f.badHeaders)
	}
	if last != 2500 {
		t.Fatalf("progress ended at %d", last)
	}
}

// An upload that never becomes available ends with its id (publish it later).
func TestV3UploadNotAvailableKeepsUploadID(t *testing.T) {
	f := newFakeNexus(t)
	f.pollsLeft = 1 << 30
	c := f.client()
	c.opts.PollTimeout = 30 * time.Millisecond
	c.opts.PollWait = func(int) time.Duration { return 5 * time.Millisecond }
	path, _ := writeArchive(t, 10)
	_, err := c.upload(t.Context(), path, nil)
	ue, ok := errors.AsType[*UploadError](err)
	if !ok || ue.UploadID != fakeUploadID || !errors.Is(err, errNotAvailable) {
		t.Fatalf("err %v", err)
	}
}

// Refusals come back as problem details, 401 as ErrBadAPIKey; reads of the
// targets parse; the key never shows in an error.
func TestV3ReadsAndErrors(t *testing.T) {
	f := newFakeNexus(t)
	c := f.client()
	m, err := c.mod(t.Context(), "wartales", 202)
	if err != nil || m.ID != "mod-uid-202" || m.Name == nil || *m.Name != "Wartales MP" {
		t.Fatalf("mod %+v %v", m, err)
	}
	files, err := c.modFiles(t.Context(), m.ID)
	if err != nil || len(files) != 1 || files[0].ID != "file-main" || !files[0].IsActive || files[0].VersionsCount != 2 {
		t.Fatalf("files %+v %v", files, err)
	}
	vs, err := c.fileVersions(t.Context(), "file-main")
	if err != nil || len(vs) != 2 || vs[1].ID != "ver-2" || !vs[1].IsPrimary || vs[1].Category != "main" {
		t.Fatalf("versions %+v %v", vs, err)
	}

	f.versionFail = http.StatusUnprocessableEntity
	_, err = c.createVersion(t.Context(), "file-main", v3VersionBody{UploadID: fakeUploadID, Name: "x", Version: "1", FileCategory: "main"})
	ve, ok := errors.AsType[*V3Error](err)
	if !ok || ve.Status != 422 || !strings.Contains(err.Error(), "version already exists") {
		t.Fatalf("422: %v", err)
	}

	f.key = "wrong-" + testKey
	_, err = c.mod(t.Context(), "wartales", 202)
	if !errors.Is(err, ErrBadAPIKey) || strings.Contains(err.Error(), f.key) {
		t.Fatalf("401: %v", err)
	}
}

// Writes send their JSON bodies once each and parse the data member.
func TestV3Writes(t *testing.T) {
	f := newFakeNexus(t)
	c := f.client()
	r, err := c.createVersion(t.Context(), "file-main", v3VersionBody{UploadID: fakeUploadID, Name: "Wartales MP", Version: "0.3.0", FileCategory: "main", ArchiveExistingFile: true})
	if err != nil || r.Version.ID != "ver-3" || r.File.GameScopedID != "777" {
		t.Fatalf("version %+v %v", r, err)
	}
	nf, err := c.createFile(t.Context(), v3FileBody{UploadID: fakeUploadID, ModID: "mod-uid-202", Name: "Wartales MP tools", Version: "0.3.0", FileCategory: "optional"})
	if err != nil || nf.ID != "file-new" || nf.GameScopedID != "778" {
		t.Fatalf("file %+v %v", nf, err)
	}
	if err := c.addChangelog(t.Context(), "mod-uid-202", "0.3.0", "Fixed sync"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := string(f.bodies["POST /mods/mod-uid-202/changelogs"]); got != `{"changelog":"Fixed sync","version":"0.3.0"}` {
		t.Fatalf("changelog body %s", got)
	}
	if f.versionPOSTs != 1 {
		t.Fatalf("version POSTs %d", f.versionPOSTs)
	}
}

func TestFileMD5(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h, n, err := fileMD5(p); err != nil || n != 5 || h != "5d41402abc4b2a76b9719d911017c592" {
		t.Fatalf("%s %d %v", h, n, err)
	}
}

// completeXML escapes ETag text.
func TestCompleteXMLEscapes(t *testing.T) {
	if got := string(completeXML([]string{"a&b"})); !strings.Contains(got, "<ETag>a&amp;b</ETag>") {
		t.Fatal(got)
	}
}
