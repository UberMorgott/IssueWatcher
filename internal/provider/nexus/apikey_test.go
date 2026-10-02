package nexus

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testKey = "test-key-AbC123=="

// fakeV1 answers users/validate for testKey only.
func fakeV1(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/users/validate" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Application-Name") != AppName || r.Header.Get("Application-Version") != "1.2.3" {
			http.Error(w, "missing application headers", http.StatusBadRequest)
			return
		}
		if r.Header.Get("apikey") != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Please provide a valid API Key"}`))
			return
		}
		_, _ = w.Write([]byte(`{"user_id":4242,"key":"` + testKey + `","name":"UberMorgott","is_premium":false}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAPIKeySaveValidateStatus(t *testing.T) {
	var calls atomic.Int32
	srv := fakeV1(t, &calls)
	dir := t.TempDir()
	k := NewKeys(KeysOptions{Dir: dir, HTTP: srv.Client(), V1: srv.URL + "/v1", Version: "1.2.3"})

	if st, err := k.Status(); err != nil || st.HasAPIKey {
		t.Fatalf("empty status %+v %v", st, err)
	}
	if _, err := k.Key(); !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("key of empty store: %v", err)
	}
	// A refused key is not stored, and the error never carries it.
	if _, err := k.Save(t.Context(), "wrong-key"); !errors.Is(err, ErrBadAPIKey) || strings.Contains(err.Error(), "wrong-key") {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, apiFile)); !os.IsNotExist(err) {
		t.Fatalf("refused key stored: %v", err)
	}
	if _, err := k.Save(t.Context(), "has space"); !errors.Is(err, ErrBadAPIKey) {
		t.Fatalf("malformed key: %v", err)
	}
	st, err := k.Save(t.Context(), "  "+testKey+"\n")
	if err != nil || !st.HasAPIKey || st.User != "UberMorgott" || st.UserID != 4242 || st.CheckedAt == "" {
		t.Fatalf("save: %+v %v", st, err)
	}
	// The file is protected (DPAPI, base64): the key is not in it as plain text.
	raw, err := os.ReadFile(filepath.Join(dir, apiFile)) //nolint:gosec // G304: test temp dir
	if err != nil || strings.Contains(string(raw), testKey) {
		t.Fatalf("stored file: %v (plain key present)", err)
	}
	if got, err := k.Key(); err != nil || got != testKey {
		t.Fatalf("key round-trip: %v", err)
	}
	if st, err := k.Check(t.Context()); err != nil || st.User != "UberMorgott" {
		t.Fatalf("check: %+v %v", st, err)
	}
	if st, err := k.Save(t.Context(), ""); err != nil || st.HasAPIKey {
		t.Fatalf("clear: %+v %v", st, err)
	}
	if st, _ := k.Status(); st.HasAPIKey {
		t.Fatal("cleared key still reported")
	}
	if n := calls.Load(); n != 3 { // wrong, good, check
		t.Fatalf("validate calls = %d", n)
	}
}
