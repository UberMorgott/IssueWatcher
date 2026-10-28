package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIOutboundVerbs(t *testing.T) {
	calls := fakeAPI(t, map[string]string{
		"POST /api/outbound/post":     `200 {"ok":true,"code":"dry_run","verb":"post","provider":"github","url":"https://github.com/o/r/issues/1","dryRun":true,"plan":{"endpoint":"GraphQL addComment(subjectId I_1)"}}`,
		"POST /api/outbound/reply":    `409 {"ok":false,"code":"reply_off","error":"nexus: replying is off"}`,
		"POST /api/outbound/read":     `502 {"ok":false,"code":"outcome_unknown","error":"unknown"}`,
		"POST /api/outbound/download": `200 {"ok":true,"code":"ok","verb":"download","provider":"nexus","url":"https://www.nexusmods.com/g/mods/1","count":1,"threads":[{"id":"bug:9"}]}`,
	})
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("\xef\xbb\xbfFixed."), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, stderr := cliRun("", "post", "--url", "https://github.com/o/r/issues/1", "--body-file", body, "--dry-run", "--json")
	if code != exitOK || strings.Count(out, "\n") != 1 || !strings.Contains(out, `"code":"dry_run"`) {
		t.Fatalf("post: %d %q %q", code, out, stderr)
	}
	var sent struct {
		URL    string `json:"url"`
		Body   string `json:"body"`
		DryRun bool   `json:"dryRun"`
	}
	if err := json.Unmarshal([]byte((*calls)[0].Body), &sent); err != nil || sent.Body != "Fixed." || !sent.DryRun || sent.URL != "https://github.com/o/r/issues/1" {
		t.Fatalf("post request %+v %v", sent, err)
	}

	// reply --url goes to the new path; the body from stdin; refusal → exit 4 + JSON error on stdout.
	code, out, _ = cliRun("thanks", "reply", "--url", "https://www.nexusmods.com/g/mods/1", "--parent", "5", "-", "--json")
	if code != exitRefused || !strings.Contains(out, `"code":"reply_off"`) || !strings.Contains(out, `"ok":false`) || (*calls)[1].Path != "/api/outbound/reply" {
		t.Fatalf("reply: %d %q %+v", code, out, (*calls)[1])
	}
	if code, out, _ = cliRun("", "read", "--url", "https://x.test/", "--json"); code != exitOutcomeUnknown || !strings.Contains(out, `"exit":5`) {
		t.Fatalf("read outcome unknown: %d %q", code, out)
	}

	dest := filepath.Join(t.TempDir(), "thread.json")
	code, out, stderr = cliRun("", "download", "--url", "https://www.nexusmods.com/g/mods/1", "--out", dest)
	b, err := os.ReadFile(filepath.Clean(dest))
	if code != exitOK || err != nil || !strings.Contains(string(b), `"bug:9"`) || !strings.Contains(out, `"bytes"`) {
		t.Fatalf("download: %d %q %q %v %q", code, out, stderr, err, b)
	}

	// Usage mistakes never reach the app.
	n := len(*calls)
	for _, args := range [][]string{
		{"post", "--body-file", body},
		{"post", "--url", "u"},
		{"post", "--url", "u", "--parent", "1", "--body-file", body},
		{"read", "--url", "u", "--dry-run"},
		{"read", "--url", "u", "--out", "x"},
		{"download", "--url", "u"},
		{"reply", "--url", "u", "--body-file", body, "-"},
	} {
		if code, _, stderr := cliRun("", args...); code != exitUsage || !strings.Contains(stderr, "usage:") {
			t.Errorf("%v: %d %q", args, code, stderr)
		}
	}
	if len(*calls) != n {
		t.Fatalf("a usage error called the app: %+v", (*calls)[n:])
	}
	// reply <itemId> without --url stays the synced-item reply.
	if isOutbound([]string{"reply", "12", "--body-file", body}) || !isOutbound([]string{"reply", "--url=https://x"}) {
		t.Fatal("isOutbound routing")
	}
}
