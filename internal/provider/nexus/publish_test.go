package nexus

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/provider"
)

var mod202 = provider.Project{ExternalID: "wartales/202", Name: "Wartales MP"}

func (f *fakeNexus) provider() *Provider {
	return New(Options{V3: &V3Options{Base: f.api.URL + "/v3", HTTP: f.api.Client(), Key: func() (string, error) { return f.key, nil },
		Version: "1.2.3", PollWait: func(int) time.Duration { return time.Millisecond }, PollTimeout: 5 * time.Second}})
}

func (f *fakeNexus) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestPublishTargets(t *testing.T) {
	f := newFakeNexus(t)
	p := f.provider()
	if !p.Capabilities().Publish || New(Options{}).Capabilities().Publish {
		t.Fatal("Capabilities.Publish must follow the v3 client")
	}
	tg, err := p.PublishTargets(t.Context(), mod202)
	if err != nil || tg.ModUID != "mod-uid-202" || tg.ModName != "Wartales MP" || len(tg.Files) != 1 ||
		tg.FilesURL != "https://www.nexusmods.com/wartales/mods/202?tab=files" {
		t.Fatalf("targets %+v %v", tg, err)
	}
	fl := tg.Files[0]
	if fl.ID != "file-main" || !fl.Active || len(fl.Versions) != 2 || fl.Versions[1].Version != "0.2.0" || !fl.Versions[1].Primary {
		t.Fatalf("file %+v", fl)
	}
}

// A new version: upload, the version POST with the exact body, changelog;
// progress stages in order.
func TestPublishVersion(t *testing.T) {
	f := newFakeNexus(t)
	path, _ := writeArchive(t, 2500)
	var mu sync.Mutex
	var stages []string
	req := provider.PublishRequest{FileID: "file-main", Path: path, Name: "Wartales MP", Version: "0.3.0",
		Description: "Co-op fixes", ArchivePrevious: true, UpdateModVersion: true, PreviousVersionID: "ver-2",
		AllowModManagerDownload: new(true), Changelog: "Fixed desync on load"}
	res, err := f.provider().Publish(t.Context(), mod202, req, func(p provider.PublishProgress) {
		mu.Lock()
		if len(stages) == 0 || stages[len(stages)-1] != p.Stage {
			stages = append(stages, p.Stage)
		}
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.UploadID != fakeUploadID || res.VersionID != "ver-3" || res.FileID != "file-main" || res.Changelog != "added" ||
		res.Size != 2500 || len(res.MD5) != 32 {
		t.Fatalf("result %+v", res)
	}
	want := []string{provider.StageResolve, provider.StageHash, provider.StageUpload, provider.StageWait, provider.StagePublish, provider.StageChangelog}
	if !slices.Equal(stages, want) {
		t.Fatalf("stages %v", stages)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body := string(f.bodies["POST /mod-files/file-main/versions"])
	wantBody := `{"upload_id":"` + fakeUploadID + `","name":"Wartales MP","description":"Co-op fixes","version":"0.3.0","file_category":"main",` +
		`"allow_mod_manager_download":true,"update_mod_version":true,"archive_existing_file":true,"previous_version_id":"ver-2"}`
	if body != wantBody {
		t.Fatalf("version body\n%s\nwant\n%s", body, wantBody)
	}
	if got := string(f.bodies["POST /mods/mod-uid-202/changelogs"]); got != `{"changelog":"Fixed desync on load","version":"0.3.0"}` {
		t.Fatalf("changelog body %s", got)
	}
	if f.versionPOSTs != 1 {
		t.Fatalf("version POSTs %d", f.versionPOSTs)
	}
}

// A refused publish is never re-sent; it returns the upload id, and a retry
// with that id publishes without uploading again.
func TestPublishFailureKeepsUploadIDNoSecondPOST(t *testing.T) {
	f := newFakeNexus(t)
	f.versionFail = 500
	path, _ := writeArchive(t, 1500)
	p := f.provider()
	req := provider.PublishRequest{FileID: "file-main", Path: path, Name: "Wartales MP", Version: "0.3.0"}
	_, err := p.Publish(t.Context(), mod202, req, nil)
	pe, ok := errors.AsType[*provider.PublishError](err)
	if !ok || pe.Stage != provider.StagePublish || pe.UploadID != fakeUploadID {
		t.Fatalf("err %v", err)
	}
	if n := f.count("POST /mod-files/file-main/versions"); n != 1 {
		t.Fatalf("version POSTs after a failure: %d", n)
	}

	f.mu.Lock()
	f.versionFail = 0
	f.mu.Unlock()
	retry := provider.PublishRequest{FileID: "file-main", UploadID: pe.UploadID, Name: "Wartales MP", Version: "0.3.0"}
	res, err := p.Publish(t.Context(), mod202, retry, nil)
	if err != nil || res.VersionID != "ver-3" || res.UploadID != fakeUploadID {
		t.Fatalf("retry %+v %v", res, err)
	}
	if n := f.count("POST /uploads/multipart"); n != 1 {
		t.Fatalf("re-uploaded: %d multipart creates", n)
	}
	if n := f.count("POST /mod-files/file-main/versions"); n != 2 {
		t.Fatalf("version POSTs %d (one per attempt)", n)
	}
}

// A dry run reads the mod and the archive, writes nothing, and plans the requests.
func TestPublishDryRunWritesNothing(t *testing.T) {
	f := newFakeNexus(t)
	path, _ := writeArchive(t, 2500)
	req := provider.PublishRequest{FileID: "file-main", Path: path, Name: "Wartales MP", Version: "0.3.0", Changelog: "x", DryRun: true}
	res, err := f.provider().Publish(t.Context(), mod202, req, nil)
	if err != nil || !res.DryRun || res.UploadID != "" || res.Size != 2500 {
		t.Fatalf("dry run %+v %v", res, err)
	}
	f.mu.Lock()
	calls := slices.Clone(f.calls)
	parts := len(f.parts)
	f.mu.Unlock()
	if len(calls) != 1 || calls[0] != "GET /games/wartales/mods/202" || parts != 0 {
		t.Fatalf("dry run sent %v (+%d parts)", calls, parts)
	}
	var methods []string
	for _, s := range res.Plan {
		methods = append(methods, s.Method+" "+strings.TrimPrefix(s.URL, f.api.URL+"/v3"))
	}
	want := []string{"POST /uploads/multipart", "PUT part_presigned_urls[*]", "POST complete_presigned_url",
		"POST /uploads/{upload_id}/finalise", "GET /uploads/{upload_id}", "POST /mod-files/file-main/versions", "POST /mods/mod-uid-202/changelogs"}
	if !slices.Equal(methods, want) {
		t.Fatalf("plan %v", methods)
	}
	b, _ := json.Marshal(res.Plan[5].Body)
	if !strings.Contains(string(b), `"upload_id":"{upload_id}"`) {
		t.Fatalf("planned body %s", b)
	}
}

func TestCheckPublish(t *testing.T) {
	f := newFakeNexus(t)
	p := f.provider()
	path, _ := writeArchive(t, 10)
	ok := provider.PublishRequest{FileID: "file-main", Path: path, Name: "Wartales MP", Version: "0.3.0"}
	if err := p.CheckPublish(mod202, ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(r *provider.PublishRequest){
		"version chars":  func(r *provider.PublishRequest) { r.Version = "v 0.3" },
		"name chars":     func(r *provider.PublishRequest) { r.Name = "Wartales/MP" },
		"name too long":  func(r *provider.PublishRequest) { r.Name = strings.Repeat("a", 51) },
		"category":       func(r *provider.PublishRequest) { r.Category = "update" },
		"relative path":  func(r *provider.PublishRequest) { r.Path = "a.zip" },
		"missing file":   func(r *provider.PublishRequest) { r.Path = path + ".gone" },
		"path+upload":    func(r *provider.PublishRequest) { r.UploadID = fakeUploadID },
		"no archive":     func(r *provider.PublishRequest) { r.Path = "" },
		"no target file": func(r *provider.PublishRequest) { r.FileID = "" },
	}
	for name, mut := range bad {
		r := ok
		mut(&r)
		if err := p.CheckPublish(mod202, r); !errors.Is(err, provider.ErrBadPublish) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := f.count("GET /games/wartales/mods/202"); n != 0 {
		t.Fatalf("CheckPublish made calls: %d", n)
	}
}
