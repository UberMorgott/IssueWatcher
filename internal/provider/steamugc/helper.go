package steamugc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// HelperArg is the app's hidden command line switch that runs the helper.
const HelperArg = "__steamugc"

// Env names the helper reads (set by the parent).
const (
	EnvDLL = "IW_STEAM_API_DLL" // absolute path of steam_api64.dll
)

// Job is the helper's input (one JSON object on stdin).
type Job struct {
	Op    string `json:"op"` // create | page | whoami
	AppID uint32 `json:"appId"`
	// ResultPath: create writes the new item id here (then fsyncs) before
	// anything else, so a lost answer never leads to a second item.
	ResultPath string `json:"resultPath,omitempty"`
	Item       uint64 `json:"item,omitempty"` // page
	Page       Page   `json:"page"`
	ChangeNote string `json:"changeNote,omitempty"`
}

// LangResult is one language's submit.
type LangResult struct {
	Language string `json:"language"`
	EResult  int    `json:"eresult"`
	OK       bool   `json:"ok"`
}

// Result is the helper's output (one JSON object on stdout).
type Result struct {
	SteamID    string       `json:"steamId,omitempty"`
	Item       uint64       `json:"item,omitempty"`
	NeedsLegal bool         `json:"needsLegalAgreement,omitempty"`
	Languages  []LangResult `json:"languages,omitempty"`
	Error      string       `json:"error,omitempty"`
	Code       string       `json:"code,omitempty"` // init | create | submit | bad_job
}

// API is the part of ISteamUGC / ISteamUser the helper uses (ffi_windows.go;
// tests: a fake).
type API interface {
	Init() error
	Shutdown()
	SteamID() uint64
	// CreateItem returns the new item, the legal-agreement flag and the EResult.
	CreateItem(appID uint32) (uint64, bool, int, error)
	// Update is one StartItemUpdate … SubmitItemUpdate round.
	Update(appID uint32, item uint64, u Update) (int, bool, error)
}

// Update is one item update: Language scopes Title / Description.
type Update struct {
	Language    string
	Title       string
	Description string
	Preview     string   // "" = unchanged
	Tags        []string // nil = unchanged
	Visibility  int      // -1 = unchanged
	ChangeNote  string
}

const eresultOK = 1

// HelperMain runs the helper: Job on stdin, Result on stdout; exit 0 when
// the result has no error.
func HelperMain(api API, in io.Reader, out io.Writer) int {
	var job Job
	res := Result{}
	if err := json.NewDecoder(io.LimitReader(in, 1<<20)).Decode(&job); err != nil {
		res.Error, res.Code = "bad job: "+err.Error(), "bad_job"
	} else {
		res = Run(api, job)
	}
	_ = json.NewEncoder(out).Encode(res)
	if res.Error != "" {
		return 1
	}
	return 0
}

// Run executes job against api.
func Run(api API, job Job) Result {
	if job.AppID == 0 {
		return Result{Error: "no app id", Code: "bad_job"}
	}
	if err := api.Init(); err != nil {
		return Result{Error: err.Error(), Code: "init"}
	}
	defer api.Shutdown()
	res := Result{SteamID: fmt.Sprint(api.SteamID())}
	switch job.Op {
	case "whoami":
		return res
	case "create":
		if job.ResultPath == "" {
			return Result{Error: "create needs a result path", Code: "bad_job"}
		}
		item, legal, er, err := api.CreateItem(job.AppID)
		if err == nil && (er != eresultOK || item == 0) {
			err = fmt.Errorf("CreateItem: EResult %d", er)
		}
		if err != nil {
			res.Error, res.Code = err.Error(), "create"
			return res
		}
		if err := writeSynced(job.ResultPath, fmt.Sprintf(`{"item":%d,"at":%q}`, item, time.Now().UTC().Format(time.RFC3339))); err != nil {
			res.Error, res.Code = fmt.Sprintf("created item %d but could not record it: %v", item, err), "record"
			res.Item = item
			return res
		}
		res.Item, res.NeedsLegal = item, legal
		return res
	case "page":
		if job.Item == 0 || len(job.Page.Texts) == 0 {
			return Result{Error: "page needs an item and texts", Code: "bad_job"}
		}
		res.Item = job.Item
		for i, t := range job.Page.Texts {
			u := Update{Language: t.Language, Title: t.Title, Description: t.Description, Visibility: -1}
			if i == 0 { // item-global fields once, with the default language
				u.Preview, u.Tags, u.Visibility, u.ChangeNote = job.Page.Preview, job.Page.Tags, job.Page.Visibility, job.ChangeNote
			}
			er, legal, err := api.Update(job.AppID, job.Item, u)
			res.NeedsLegal = res.NeedsLegal || legal
			res.Languages = append(res.Languages, LangResult{Language: t.Language, EResult: er, OK: err == nil && er == eresultOK})
			if err != nil && res.Error == "" {
				res.Error, res.Code = fmt.Sprintf("%s: %v", t.Language, err), "submit"
			}
		}
		if res.Error == "" {
			for _, l := range res.Languages {
				if !l.OK {
					res.Error, res.Code = fmt.Sprintf("%s: EResult %d", l.Language, l.EResult), "submit"
					break
				}
			}
		}
		return res
	}
	return Result{Error: "unknown op " + job.Op, Code: "bad_job"}
}

func writeSynced(path, s string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the parent's result path
	if err != nil {
		return err
	}
	_, err = f.WriteString(s)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
