package curseforge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

const testToken = "abcd1234-ef56-7890-abcd-1234567890ab"

// fakeCF is the upload API and CFWidget for one Hytale project.
type fakeCF struct {
	t        *testing.T
	mu       sync.Mutex
	uploads  []fakeUpload
	widget   []widgetFile
	failNext int // upload-file answers: 0 ok, 500 server error, -1 drop the connection
}

type fakeUpload struct {
	token, metadata, fileName string
	file                      []byte
}

func (f *fakeCF) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("token") != "" {
			f.t.Errorf("token in the URL")
		}
		if r.Header.Get("X-Api-Token") != testToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errorCode":401,"errorMessage":"You must provide an API token"}`)
			return false
		}
		return true
	}
	versions := func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		_, _ = io.WriteString(w, `[{"id":10,"gameVersionTypeID":1,"name":"Early Access","slug":"early-access"},{"id":11,"gameVersionTypeID":1,"name":"0.6","slug":"0-6"}]`)
	}
	mux.HandleFunc("GET /api/game/hytale/versions", versions)
	mux.HandleFunc("GET /api/game/minecraft/versions", versions)
	mux.HandleFunc("POST /api/projects/1443010/upload-file", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.ContentLength <= 0 {
			f.t.Errorf("no Content-Length")
		}
		mr, err := r.MultipartReader()
		if err != nil {
			f.t.Fatal(err)
		}
		var up fakeUpload
		up.token = r.Header.Get("X-Api-Token")
		var order []string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				f.t.Fatal(err)
			}
			b, _ := io.ReadAll(p)
			order = append(order, p.FormName())
			switch p.FormName() {
			case "metadata":
				up.metadata = string(b)
			case "file":
				up.fileName, up.file = p.FileName(), b
			}
		}
		if strings.Join(order, ",") != "metadata,file" {
			f.t.Errorf("parts %v", order)
		}
		f.mu.Lock()
		f.uploads = append(f.uploads, up)
		fail := f.failNext
		f.failNext = 0
		f.mu.Unlock()
		switch fail {
		case 500:
			w.WriteHeader(http.StatusInternalServerError)
			return
		case -1:
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			_ = c.Close()
			return
		case 400:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"errorCode":1018,"errorMessage":"Invalid game version"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":9000001}`)
	})
	mux.HandleFunc("GET /widget/1443010", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		files := f.widget
		f.mu.Unlock()
		b, _ := json.Marshal(map[string]any{"id": 1443010, "title": "Crossbow Save Arrow", "game": "hytale",
			"urls": map[string]string{"curseforge": "https://www.curseforge.com/hytale/mods/crossbow-save-arrow"}, "files": files})
		_, _ = w.Write(b)
	})
	return mux
}

func newFakeCF(t *testing.T) (*fakeCF, *Uploader, string) {
	t.Helper()
	f := &fakeCF{t: t, widget: []widgetFile{
		{ID: 7585785, Display: "CrossbowSaveArrow-0.0.8.jar", Name: "CrossbowSaveArrow-0.0.8.jar", Type: "release", Versions: []string{"Early Access"}, UploadedAt: "2026-02-06T19:34:42.000Z"},
		{ID: 8953746, Display: "CrossbowSaveArrow 0.0.9", Name: "CrossbowSaveArrow-0.0.9.jar", Type: "release", Versions: []string{"0.6"}, UploadedAt: "2026-09-23T08:23:17.077Z"},
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	u := NewUploader(UploadOptions{Dir: t.TempDir(), HTTP: srv.Client(), Site: srv.URL, Widget: srv.URL + "/widget", Now: func() time.Time { return now }})
	jar := filepath.Join(t.TempDir(), "CrossbowSaveArrow-0.1.0.jar")
	if err := os.WriteFile(jar, []byte("PK\x03\x04 jar bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, u, jar
}

var cfProject = provider.Project{ExternalID: "1443010"}

func TestUploadTokenSaveValidates(t *testing.T) {
	_, u, _ := newFakeCF(t)
	if _, err := u.Save(context.Background(), "wrong-token-123456"); !errors.Is(err, ErrBadToken) || !errors.Is(err, provider.ErrUploadAuthRefused) {
		t.Fatalf("bad token: %v", err)
	}
	if st, _ := u.Status(); st.HasToken {
		t.Fatal("a refused token was stored")
	}
	st, err := u.Save(context.Background(), testToken)
	if err != nil || !st.HasToken || st.CheckedAt == "" {
		t.Fatalf("save: %+v %v", st, err)
	}
	b, _ := os.ReadFile(filepath.Join(u.opts.Dir, tokenFile))
	if strings.Contains(string(b), testToken) {
		t.Fatal("token stored in clear text")
	}
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Save(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Check(context.Background()); !errors.Is(err, ErrNoToken) || !errors.Is(err, provider.ErrNoUploadAuth) {
		t.Fatalf("no token: %v", err)
	}
}

func TestUploadPublishExactPayload(t *testing.T) {
	f, u, jar := newFakeCF(t)
	req := provider.PublishRequest{Path: jar, Version: "0.1.0", Name: "Crossbow Save Arrow", Changelog: "Fixes #3"}
	if _, err := u.Publish(context.Background(), cfProject, req, nil); !errors.Is(err, ErrNoToken) {
		t.Fatalf("no token: %v", err)
	}
	if _, err := u.Save(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	dry := req
	dry.DryRun = true
	res, err := u.Publish(context.Background(), cfProject, dry, nil)
	if err != nil || len(res.Plan) != 2 || len(f.uploads) != 0 {
		t.Fatalf("dry run: %+v %v uploads %d", res, err, len(f.uploads))
	}
	res, err = u.Publish(context.Background(), cfProject, req, nil)
	if err != nil || res.VersionID != "9000001" || res.Size != int64(len("PK\x03\x04 jar bytes")) {
		t.Fatalf("publish: %+v %v", res, err)
	}
	if len(f.uploads) != 1 {
		t.Fatalf("uploads %d", len(f.uploads))
	}
	up := f.uploads[0]
	want := `{"changelog":"Fixes #3","changelogType":"markdown","displayName":"Crossbow Save Arrow 0.1.0","gameVersions":[11],"releaseType":"release"}`
	if up.metadata != want || up.fileName != "CrossbowSaveArrow-0.1.0.jar" || string(up.file) != "PK\x03\x04 jar bytes" {
		t.Fatalf("payload %q %q %q", up.metadata, up.fileName, up.file)
	}

	// In moderation: listed as pending; a second upload of it is refused.
	pt, err := u.PublishTargets(context.Background(), cfProject)
	if err != nil {
		t.Fatal(err)
	}
	vs := pt.Files[0].Versions
	if last := vs[len(vs)-1]; last.Version != "0.1.0" || !last.Pending || last.ID != "9000001" {
		t.Fatalf("pending version: %+v", vs)
	}
	if _, err := u.Publish(context.Background(), cfProject, req, nil); !errors.Is(err, provider.ErrBadPublish) || len(f.uploads) != 1 {
		t.Fatalf("second publish: %v uploads %d", err, len(f.uploads))
	}
	// Approved: CFWidget lists it, no longer pending.
	f.mu.Lock()
	f.widget = append(f.widget, widgetFile{ID: 9000001, Display: "Crossbow Save Arrow 0.1.0", Name: "CrossbowSaveArrow-0.1.0.jar", Type: "release", Versions: []string{"0.6"}, UploadedAt: "2026-10-05T12:00:00Z"})
	f.mu.Unlock()
	pt, _ = u.PublishTargets(context.Background(), cfProject)
	vs = pt.Files[0].Versions
	if last := vs[len(vs)-1]; last.Version != "0.1.0" || last.Pending || len(vs) != 3 {
		t.Fatalf("approved version: %+v", vs)
	}
}

func TestUploadGameVersionsByNameAndType(t *testing.T) {
	f, u, jar := newFakeCF(t)
	if _, err := u.Save(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	req := provider.PublishRequest{Path: jar, Version: "0.1.0", Name: "CrossbowSaveArrow 0.1.0", GameVersions: []string{"Early Access", "11"}, ReleaseType: "beta"}
	if _, err := u.Publish(context.Background(), cfProject, req, nil); err != nil {
		t.Fatal(err)
	}
	want := `{"changelog":"Version 0.1.0","changelogType":"markdown","displayName":"CrossbowSaveArrow 0.1.0","gameVersions":[10,11],"releaseType":"beta"}`
	if f.uploads[0].metadata != want {
		t.Fatalf("metadata %s", f.uploads[0].metadata)
	}
	bad := req
	bad.Version, bad.GameVersions = "0.1.1", []string{"9.9"}
	if _, err := u.Publish(context.Background(), cfProject, bad, nil); !errors.Is(err, provider.ErrBadPublish) || len(f.uploads) != 1 {
		t.Fatalf("unknown game version: %v", err)
	}
	if err := u.CheckPublish(cfProject, provider.PublishRequest{Path: jar, Version: "0.1.0", ReleaseType: "stable"}); !errors.Is(err, provider.ErrBadPublish) {
		t.Fatalf("release type: %v", err)
	}
}

func TestUploadLostAnswerIsUnknown(t *testing.T) {
	f, u, jar := newFakeCF(t)
	if _, err := u.Save(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	f.failNext = -1
	req := provider.PublishRequest{Path: jar, Version: "0.1.0", Name: "Crossbow Save Arrow"}
	if _, err := u.Publish(context.Background(), cfProject, req, nil); err == nil {
		t.Fatal("dropped connection: no error")
	}
	// The probe must not call it absent.
	if _, err := u.PublishTargets(context.Background(), cfProject); !errors.Is(err, errUploadUnknown) {
		t.Fatalf("probe after a lost answer: %v", err)
	}
	if _, err := u.Publish(context.Background(), cfProject, req, nil); err == nil || len(f.uploads) != 1 {
		t.Fatalf("resend after a lost answer: %v uploads %d", err, len(f.uploads))
	}
}

func TestUploadRefusedLeavesNoJournal(t *testing.T) {
	f, u, jar := newFakeCF(t)
	if _, err := u.Save(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	f.failNext = 400
	req := provider.PublishRequest{Path: jar, Version: "0.1.0", Name: "Crossbow Save Arrow"}
	if _, err := u.Publish(context.Background(), cfProject, req, nil); err == nil || !strings.Contains(err.Error(), "Invalid game version") {
		t.Fatalf("refused upload: %v", err)
	}
	if _, err := u.PublishTargets(context.Background(), cfProject); err != nil {
		t.Fatalf("a refused upload is absent: %v", err)
	}
	if _, err := u.Publish(context.Background(), cfProject, req, nil); err != nil || len(f.uploads) != 2 {
		t.Fatalf("retry after refusal: %v", err)
	}
}
