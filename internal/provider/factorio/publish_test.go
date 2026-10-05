package factorio

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/signin"
	"github.com/UberMorgott/issuewatcher/internal/websession"
)

const testKey = "k3yABCdef0123456789abcdef0123456789abcdef012"

// fakeUpload is the mod portal's upload API and factorio.com's key form.
type fakeUpload struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	calls    map[string]int
	releases []string
	initBody map[string][]string // init_upload form fields
	auth     string              // init_upload Authorization
	file     []byte              // uploaded archive
	fileName string
	keyForm  url.Values // create-api-key POST body
	failInit string     // init_upload error code
	failUp   bool       // the upload answers 500
}

func newFakeUpload(t *testing.T) *fakeUpload {
	f := &fakeUpload{t: t, calls: map[string]int{}, releases: []string{"1.1.4", "1.1.5"}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpload) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[call]
}

const createForm = `<form id="create_api_key_form" method="POST" action="/create-api-key"><dl><label><dt>Usages</dt><dd>
<label class="checkbox-label"> <input id="usages-0" name="usages" type="checkbox" value="u-upload"> <div class="checkbox"></div> <div> ModPortal: Upload Mods </div> </label>
<label class="checkbox-label"> <input id="usages-1" name="usages" type="checkbox" value="u-publish"> <div class="checkbox"></div> <div> ModPortal: Publish Mods </div> </label>
</dd></label><label><dt>Note</dt><dd><input id="note" maxlength="300" name="note" type="text" value=""></dd></label></dl>
<input id="csrf_token" name="csrf_token" type="hidden" value="csrf-1"></form>`

func (f *fakeUpload) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	call := r.Method + " " + r.URL.Path
	f.calls[call]++
	switch call {
	case "GET /api/mods/auto-build-and-deconstruct":
		var rel []string
		for _, v := range f.releases {
			rel = append(rel, fmt.Sprintf(`{"version":%q,"released_at":"2026-09-28T21:08:10.000000Z","file_name":"auto-build-and-deconstruct_%s.zip","sha1":"sha1-of-%s"}`, v, v, v))
		}
		_, _ = fmt.Fprintf(w, `{"name":"auto-build-and-deconstruct","title":"Lazy Builder","owner":"Morgott","releases":[%s]}`, strings.Join(rel, ","))
	case "POST " + initUploadPath:
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: body bounded by MaxBytesReader
			f.t.Errorf("init_upload: %v", err)
		}
		f.initBody, f.auth = r.MultipartForm.Value, r.Header.Get("Authorization")
		if f.failInit != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprintf(w, `{"error":%q,"message":"nope"}`, f.failInit)
			return
		}
		_, _ = fmt.Fprintf(w, `{"upload_url":%q}`, f.srv.URL+"/upload/abc")
	case "POST /upload/abc":
		if r.ContentLength <= 0 {
			f.t.Errorf("upload: no Content-Length")
		}
		if r.Header.Get("Authorization") != "" {
			f.t.Errorf("upload: the key went to the upload URL")
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil { //nolint:gosec // G120: body bounded by MaxBytesReader
			f.t.Errorf("upload: %v", err)
			return
		}
		if len(r.MultipartForm.Value) != 0 || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
			f.t.Errorf("upload fields = %v / %v", r.MultipartForm.Value, r.MultipartForm.File)
			return
		}
		fh := r.MultipartForm.File["file"][0]
		rd, _ := fh.Open()
		f.file, _ = io.ReadAll(rd)
		f.fileName = fh.Filename
		if f.failUp {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"InternalError","message":"boom"}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true}`)
	case "GET " + createPath:
		if !strings.Contains(r.Header.Get("Cookie"), "session=ok") {
			http.Redirect(w, r, "/login?next=%2Fcreate-api-key", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, createForm)
	case "GET /login":
		_, _ = io.WriteString(w, "<form>login</form>")
	case "POST " + createPath:
		b, _ := io.ReadAll(r.Body)
		f.keyForm, _ = url.ParseQuery(string(b))
		_, _ = fmt.Fprintf(w, `<h2>Your new API key has been created. </h2> This is the only time it will be shown to you.
<div id="created_api_key" class="panel-inset mb0"> <code>%s</code> </div>`, testKey)
	default:
		f.t.Errorf("unexpected %s", call)
		http.NotFound(w, r)
	}
}

func (f *fakeUpload) provider(t *testing.T, signedIn bool) (*Provider, *Keys) {
	keys := NewKeys(KeysOptions{Dir: t.TempDir(), HTTP: f.srv.Client(), Site: f.srv.URL})
	u, _ := url.Parse(f.srv.URL)
	var cookies []websession.Cookie
	if signedIn {
		cookies = []websession.Cookie{{Name: "session", Value: "ok", Domain: u.Hostname(), Path: "/"}}
	}
	jar := websession.Memory(cookies, "UA")
	return New(Options{HTTP: f.srv.Client(), Site: f.srv.URL, Keys: keys, Session: signin.New(signin.Spec{Platform: Platform}, jar, nil)}), keys
}

// modZip writes a mod archive with info.json {name, version}.
func modZip(t *testing.T, name, version string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name+"_"+version+".zip")
	out, err := os.Create(p) //nolint:gosec // G304: test temp file
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	w, _ := zw.Create(name + "_" + version + "/info.json")
	_, _ = fmt.Fprintf(w, `{"name":%q,"version":%q,"factorio_version":"2.0"}`, name, version)
	w, _ = zw.Create(name + "_" + version + "/control.lua")
	_, _ = io.WriteString(w, "-- mod")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	return p
}

var lazy = provider.Project{ExternalID: "auto-build-and-deconstruct", Name: "Lazy Builder"}

func TestFactorioPublishCreatesKeyAndUploadsOnce(t *testing.T) {
	f := newFakeUpload(t)
	p, keys := f.provider(t, true)
	zipPath := modZip(t, "auto-build-and-deconstruct", "1.1.6")
	want, _ := os.ReadFile(zipPath) //nolint:gosec // G304: test temp file

	var stages []string
	res, err := p.Publish(context.Background(), lazy, provider.PublishRequest{Path: zipPath, Version: "1.1.6"}, func(pp provider.PublishProgress) {
		if len(stages) == 0 || stages[len(stages)-1] != pp.Stage {
			stages = append(stages, pp.Stage)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// The key form: exactly the upload usage, the note and the csrf token.
	if got := f.keyForm; len(got) != 4 || got.Get("usages") != "u-upload" || len(got["usages"]) != 1 || got.Get("csrf_token") != "csrf-1" ||
		got.Get("note") != keyNote || !got.Has("action") {
		t.Fatalf("create-api-key body = %v", got)
	}
	if st, _ := keys.Status(); !st.HasAPIKey || st.Source != "created" {
		t.Fatalf("key status = %+v", st)
	}
	if f.auth != "Bearer "+testKey || len(f.initBody) != 1 || len(f.initBody["mod"]) != 1 || f.initBody["mod"][0] != "auto-build-and-deconstruct" {
		t.Fatalf("init_upload = %q %v", f.auth, f.initBody)
	}
	if string(f.file) != string(want) || f.fileName != "auto-build-and-deconstruct_1.1.6.zip" {
		t.Fatalf("uploaded %d bytes as %q, want %d", len(f.file), f.fileName, len(want))
	}
	if f.count("POST /upload/abc") != 1 || f.count("POST "+createPath) != 1 {
		t.Fatalf("calls = %v", f.calls)
	}
	if res.VersionID != "1.1.6" || res.FileID != "auto-build-and-deconstruct" || res.Size != int64(len(want)) || res.MD5 == "" {
		t.Fatalf("res = %+v", res)
	}
	if strings.Join(stages, ",") != "resolve,hash,publish" {
		t.Fatalf("stages = %v", stages)
	}

	// A second publish reuses the stored key: no new key.
	f.releases = append(f.releases, "1.1.6")
	zip7 := modZip(t, "auto-build-and-deconstruct", "1.1.7")
	if _, err := p.Publish(context.Background(), lazy, provider.PublishRequest{Path: zip7, Version: "1.1.7"}, nil); err != nil {
		t.Fatal(err)
	}
	if f.count("POST "+createPath) != 1 || f.count("POST /upload/abc") != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestFactorioPublishDryRunWritesNothing(t *testing.T) {
	f := newFakeUpload(t)
	p, keys := f.provider(t, true)
	res, err := p.Publish(context.Background(), lazy, provider.PublishRequest{Path: modZip(t, "auto-build-and-deconstruct", "1.1.6"), Version: "1.1.6", DryRun: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Plan) != 4 || res.Plan[2].URL != f.srv.URL+initUploadPath || res.Plan[3].URL != "{upload_url}" {
		b, _ := json.Marshal(res.Plan)
		t.Fatalf("plan = %s", b)
	}
	for call, n := range f.calls {
		if !strings.HasPrefix(call, "GET /api/mods/") && n > 0 {
			t.Fatalf("dry run sent %s", call)
		}
	}
	if st, _ := keys.Status(); st.HasAPIKey {
		t.Fatal("dry run created a key")
	}
	// With a stored key the plan is the two upload calls.
	_, _ = keys.Save(testKey, "pasted", "")
	res, _ = p.Publish(context.Background(), lazy, provider.PublishRequest{Path: modZip(t, "auto-build-and-deconstruct", "1.1.6"), Version: "1.1.6", DryRun: true}, nil)
	if len(res.Plan) != 2 {
		t.Fatalf("plan = %+v", res.Plan)
	}
}

func TestFactorioPublishRefusals(t *testing.T) {
	f := newFakeUpload(t)
	p, keys := f.provider(t, false)
	good := modZip(t, "auto-build-and-deconstruct", "1.1.6")
	for name, req := range map[string]provider.PublishRequest{
		"version mismatch": {Path: good, Version: "1.1.7"},
		"other mod":        {Path: modZip(t, "other-mod", "1.1.6"), Version: "1.1.6"},
		"bad version":      {Path: good, Version: "1.1"},
		"relative":         {Path: "x.zip", Version: "1.1.6"},
		"new file":         {Path: good, Version: "1.1.6", NewFile: true},
		"changelog":        {Path: good, Version: "1.1.6", Changelog: "x"},
		"upload id":        {UploadID: "u", Version: "1.1.6"},
	} {
		if err := p.CheckPublish(lazy, req); !errors.Is(err, provider.ErrBadPublish) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Already on the portal: refused before the key or any upload.
	if _, err := p.Publish(context.Background(), lazy, provider.PublishRequest{Path: modZip(t, "auto-build-and-deconstruct", "1.1.5"), Version: "1.1.5"}, nil); !errors.Is(err, provider.ErrBadPublish) {
		t.Fatalf("duplicate: %v", err)
	}
	// Signed out and no key: ErrNoAPIKey, nothing uploaded.
	if _, err := p.Publish(context.Background(), lazy, provider.PublishRequest{Path: good, Version: "1.1.6"}, nil); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("signed out: %v", err)
	}
	// A refused key: ErrBadAPIKey, nothing uploaded.
	_, _ = keys.Save(testKey, "pasted", "")
	f.failInit = "InvalidApiKey"
	if _, err := p.Publish(context.Background(), lazy, provider.PublishRequest{Path: good, Version: "1.1.6"}, nil); !errors.Is(err, ErrBadAPIKey) {
		t.Fatalf("bad key: %v", err)
	}
	// A failed upload is a publish-stage error and is not resent.
	f.failInit, f.failUp = "", true
	_, err := p.Publish(context.Background(), lazy, provider.PublishRequest{Path: good, Version: "1.1.6"}, nil)
	var pe *provider.PublishError
	if !errors.As(err, &pe) || pe.Stage != provider.StagePublish || f.count("POST /upload/abc") != 1 || f.count("POST "+createPath) != 0 {
		t.Fatalf("upload failure: %v, calls %v", err, f.calls)
	}
}

func TestFactorioPublishTargets(t *testing.T) {
	f := newFakeUpload(t)
	p, _ := f.provider(t, false)
	tg, err := p.PublishTargets(context.Background(), lazy)
	if err != nil || len(tg.Files) != 1 || tg.Files[0].ID != "auto-build-and-deconstruct" || len(tg.Files[0].Versions) != 2 || tg.Files[0].Versions[1].Version != "1.1.5" ||
		tg.Files[0].Versions[1].SHA1 != "sha1-of-1.1.5" {
		t.Fatalf("targets = %+v, %v", tg, err)
	}
	if !p.Capabilities().Publish || New(Options{}).Capabilities().Publish {
		t.Fatal("Capabilities.Publish follows Options.Keys")
	}
}
