package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider/nexus"
)

// The Nexus API key is write-only: PUT validates it against users/validate
// (a fake v1), and no response ever carries it.
func TestNexusAPIKeyWriteOnly(t *testing.T) {
	const key = "secret-nexus-key-123"
	v1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/validate" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("apikey") != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"user_id":7,"name":"UberMorgott"}`))
	}))
	t.Cleanup(v1.Close)
	keys := nexus.NewKeys(nexus.KeysOptions{Dir: filepath.Join(t.TempDir(), "secrets"), HTTP: v1.Client(), V1: v1.URL + "/v1"})
	e := newEnv(t, func(o *Options) { o.NexusKey = keys })

	raw := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, e.s.BaseURL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := e.browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(b), key) {
			t.Fatalf("%s %s leaked the key: %s", method, path, b)
		}
		return resp.StatusCode, string(b)
	}

	if code, body := raw(http.MethodGet, "/api/providers/nexus", ""); code != http.StatusOK || !strings.Contains(body, `"hasApiKey":false`) {
		t.Fatalf("empty: %d %s", code, body)
	}
	if code, body := raw(http.MethodPost, "/api/providers/nexus/check", ""); code != http.StatusConflict || !strings.Contains(body, "no_api_key") {
		t.Fatalf("check without key: %d %s", code, body)
	}
	if code, body := raw(http.MethodPut, "/api/providers/nexus", `{"apiKey":"nope"}`); code != http.StatusBadRequest || !strings.Contains(body, "bad_api_key") {
		t.Fatalf("refused key: %d %s", code, body)
	}
	if code, body := raw(http.MethodPut, "/api/providers/nexus", `{}`); code != http.StatusBadRequest {
		t.Fatalf("no apiKey field: %d %s", code, body)
	}
	if code, body := raw(http.MethodPut, "/api/providers/nexus", `{"apiKey":"`+key+`"}`); code != http.StatusOK ||
		!strings.Contains(body, `"hasApiKey":true`) || !strings.Contains(body, `"user":"UberMorgott"`) {
		t.Fatalf("save: %d %s", code, body)
	}
	if code, body := raw(http.MethodGet, "/api/providers/nexus", ""); code != http.StatusOK || !strings.Contains(body, `"user":"UberMorgott"`) {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, body := raw(http.MethodPost, "/api/providers/nexus/check", ""); code != http.StatusOK || !strings.Contains(body, `"userId":7`) {
		t.Fatalf("check: %d %s", code, body)
	}
	if code, body := raw(http.MethodPut, "/api/providers/nexus", `{"apiKey":""}`); code != http.StatusOK || !strings.Contains(body, `"hasApiKey":false`) {
		t.Fatalf("remove: %d %s", code, body)
	}
}
