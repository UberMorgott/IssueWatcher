package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/secret"
)

// fakeV1Changelogs answers users/validate and the changelogs.json of
// wartales/202 for testKey only; changelogs is the JSON served.
func fakeV1Changelogs(t *testing.T, changelogs *atomic.Value, seen *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("apikey") != testKey || r.Header.Get("Application-Name") != AppName || r.Header.Get("Application-Version") != "1.2.3" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Please provide a valid API Key"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/users/validate":
			_, _ = w.Write([]byte(`{"user_id":4242,"name":"UberMorgott"}`))
		case "/v1/games/wartales/mods/202/changelogs.json":
			seen.Add(1)
			body, _ := changelogs.Load().(string)
			_, _ = w.Write([]byte(body))
		case "/v1/echo":
			// a server error that echoes the request's key back
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"bad key ` + r.Header.Get("apikey") + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeKeyForTest stores key as Save would, without validating it (a key
// revoked after it was saved).
func writeKeyForTest(k *Keys, key string) error {
	return secret.WriteProtectedJSON(k.path(), keyFile{APIKey: key})
}

func savedKeys(t *testing.T, srv *httptest.Server) *Keys {
	t.Helper()
	k := NewKeys(KeysOptions{Dir: t.TempDir(), HTTP: srv.Client(), V1: srv.URL + "/v1", Version: "1.2.3"})
	if _, err := k.Save(t.Context(), testKey); err != nil {
		t.Fatal(err)
	}
	return k
}

// Every v1 read carries the stored key; without one it never goes out and
// the error points to Settings › Платформы › Nexus.
func TestV1SendsStoredKey(t *testing.T) {
	var cl atomic.Value
	cl.Store(`{"0.2.4":["a","b"]}`)
	var seen atomic.Int32
	srv := fakeV1Changelogs(t, &cl, &seen)

	empty := NewKeys(KeysOptions{Dir: t.TempDir(), HTTP: srv.Client(), V1: srv.URL + "/v1", Version: "1.2.3"})
	if _, err := empty.v1Changelogs(t.Context(), "wartales", 202); !errors.Is(err, ErrNoAPIKey) || !strings.Contains(err.Error(), "Настройки › Платформы › Nexus") {
		t.Fatalf("no key: %v", err)
	}
	if seen.Load() != 0 {
		t.Fatal("request sent without a key")
	}

	k := savedKeys(t, srv)
	got, err := k.v1Changelogs(t.Context(), "wartales", 202)
	if err != nil || strings.Join(got["0.2.4"], "|") != "a|b" || seen.Load() != 1 {
		t.Fatalf("changelogs = %v %v (%d)", got, err, seen.Load())
	}

	// A refused key: ErrBadAPIKey with the hint.
	bad := NewKeys(KeysOptions{Dir: t.TempDir(), HTTP: srv.Client(), V1: srv.URL + "/v1", Version: "1.2.3"})
	if err := writeKeyForTest(bad, "revoked-key-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.v1Changelogs(t.Context(), "wartales", 202); !errors.Is(err, ErrBadAPIKey) ||
		!strings.Contains(err.Error(), "Настройки › Платформы › Nexus") || strings.Contains(err.Error(), "revoked-key-123") {
		t.Fatalf("bad key: %v", err)
	}
}

// The key never reaches an error, even when the server echoes it.
func TestV1RedactsKey(t *testing.T) {
	var cl atomic.Value
	cl.Store(`{}`)
	var seen atomic.Int32
	srv := fakeV1Changelogs(t, &cl, &seen)
	k := savedKeys(t, srv)
	var out any
	err := k.getV1(t.Context(), "/echo", &out)
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("echo error = %v", err)
	}
	// A transport error naming the key (as in a URL) is redacted too.
	if got := redactErr(errors.New("dial https://x/?k="+testKey), testKey).Error(); strings.Contains(got, testKey) {
		t.Fatalf("redactErr = %q", got)
	}
	// v3 problem details echoing the key.
	e := &V3Error{Status: http.StatusUnauthorized, Method: "GET", Path: "/x", Detail: redact("key "+testKey, testKey)}
	if s := e.Error(); strings.Contains(s, testKey) || !strings.Contains(s, "Настройки › Платформы › Nexus") || !errors.Is(e, ErrBadAPIKey) {
		t.Fatalf("v3 401 = %q", s)
	}
}

// After a set the result carries the v1 read-back per version; a cached
// (stale) answer is a mismatch with a note, not an error.
func TestSetChangelogReadsBackV1(t *testing.T) {
	var cl atomic.Value
	cl.Store(`{"0.2.10":["y"],"0.2.9":["Added: x<br />more"],"0.2.4":["Fixed: one","Changed: two &amp; &lt;three&gt;"]}`)
	var seen atomic.Int32
	srv := fakeV1Changelogs(t, &cl, &seen)
	p, _ := newChangelogNative(t, documentation202, postOK)
	p.opts.Keys = savedKeys(t, srv)

	res, err := p.SetChangelog(context.Background(), project202, provider.ChangelogEdit{Version: "0.2.4", Lines: []string{"Fixed: one", "Changed: two & <three>"}})
	if err != nil || !res.Saved || res.Check == nil {
		t.Fatalf("set = %+v %v", res, err)
	}
	b, _ := json.Marshal(res.Check)
	if !res.Check.Match || res.Check.Note != "" || len(res.Check.Versions) != 3 || !res.Check.Versions[2].Edited ||
		res.Check.Versions[2].Expected != 2 || res.Check.Versions[2].Public != 2 || strings.Contains(string(b), testKey) ||
		res.Check.Source != srv.URL+"/v1/games/wartales/mods/202/changelogs.json" {
		t.Fatalf("check = %s", b)
	}

	// Stale public copy: mismatch reported per version, with the cache note.
	cl.Store(`{"0.2.10":["y"],"0.2.9":["Added: x more"],"0.2.4":["old a","old b"]}`)
	res, err = p.DeleteChangelog(context.Background(), project202, "0.2.4", false)
	if err != nil || !res.Saved || res.Check == nil || res.Check.Match || res.Check.Note == "" {
		t.Fatalf("delete = %+v %v", res, err)
	}
	for _, v := range res.Check.Versions {
		if v.Version == "0.2.4" && (v.Match || v.Expected != 0 || v.Public != 2 || !v.Edited) {
			t.Fatalf("0.2.4 = %+v", v)
		}
		if v.Version != "0.2.4" && !v.Match {
			t.Fatalf("%s = %+v", v.Version, v)
		}
	}

	// No key: the change stands, the check says where to set the key.
	p.opts.Keys = NewKeys(KeysOptions{Dir: t.TempDir(), HTTP: srv.Client(), V1: srv.URL + "/v1"})
	res, err = p.SetChangelog(context.Background(), project202, provider.ChangelogEdit{Version: "0.2.3", Lines: []string{"n"}})
	if err != nil || !res.Saved || res.Check == nil || res.Check.Code != "no_api_key" || !strings.Contains(res.Check.Error, "Настройки › Платформы › Nexus") {
		t.Fatalf("no key = %+v %v", res, err)
	}

	// The standalone check (read-only): editor (unchanged in the fake) vs v1.
	p.opts.Keys = savedKeys(t, srv)
	c, err := p.CheckChangelogs(context.Background(), project202)
	if err != nil || !c.Match || len(c.Versions) != 3 || c.Versions[2].Expected != 2 || c.Versions[2].Edited {
		t.Fatalf("check = %+v %v", c, err)
	}
}

func TestCompareChangelogsExtraVersion(t *testing.T) {
	c := compareChangelogs("s", []provider.ChangelogVersion{{Version: "1.0", Entries: []provider.ChangelogEntry{{Text: "a"}}}},
		map[string][]string{"1.0": {"a"}, "0.9": {"z"}}, "")
	if c.Match || len(c.Versions) != 2 || c.Versions[1].Version != "0.9" || c.Versions[1].Public != 1 || len(c.Versions[1].Extra) != 1 {
		t.Fatalf("compare = %+v", c)
	}
}
