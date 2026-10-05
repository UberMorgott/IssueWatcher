package api

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/runner"
)

// A real child process inside a runner job object (curl.exe started through
// runner.RunCommand, as an agent's shell would run it) is refused a
// non-dry-run publish with the bearer token; the same request from outside
// any job object goes through.
func TestAgentCallerRefusedFromJobObject(t *testing.T) {
	curl, err := exec.LookPath("curl.exe")
	if err != nil {
		t.Skip("curl.exe not found")
	}
	pub := &fakePublisher{}
	e, id := releaseEnv(t, func(o *Options) { o.Publishers = func(string) provider.Publisher { return pub } })
	dir := t.TempDir()
	body := filepath.Join(dir, "body.json")
	if err := os.WriteFile(body, []byte(`{"version": "1.0.0", "fileId": "f", "path": "C:/a.zip"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.json")
	cfg := filepath.Join(dir, "curl.cfg")
	url := fmt.Sprintf("%s/api/projects/%d/publish", e.s.BaseURL(), id)
	lines := []string{
		`url = "` + url + `"`, `request = "POST"`,
		`header = "Authorization: Bearer ` + e.s.Token() + `"`, `header = "Content-Type: application/json"`,
		`data-binary = "@` + filepath.ToSlash(body) + `"`, `output = "` + filepath.ToSlash(out) + `"`,
		`write-out = "status=%{http_code}"`, `silent`,
	}
	if err := os.WriteFile(cfg, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runner.New(runner.Options{Store: e.store, DataDir: filepath.Join(dir, "data"), Settings: config.Defaults})
	res := r.RunCommand(t.Context(), dir, curl+" -K "+filepath.ToSlash(cfg), filepath.Join(dir, "logs"), time.Minute)
	got, _ := os.ReadFile(out) //nolint:gosec // test file
	if !strings.Contains(res.Output, "status=403") || !strings.Contains(string(got), `"code":"`+codeAgentCaller+`"`) {
		t.Fatalf("from the job object: %+v body %s", res, got)
	}
	pub.mu.Lock()
	n := len(pub.reqs)
	pub.mu.Unlock()
	if n != 0 {
		t.Fatalf("publisher called %d times", n)
	}

	// The same request from this process (no job object) is accepted.
	b, _ := os.ReadFile(body) //nolint:gosec // test file
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+e.s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("outside a job object: %d", resp.StatusCode)
	}
}
