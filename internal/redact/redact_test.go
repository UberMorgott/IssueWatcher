package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://127.0.0.1:5000/auth?t=0123abcd", "http://127.0.0.1:5000/auth?t=***"},
		{"url=http://h/auth?t=abc&next=/item/4", "url=http://h/auth?t=***&next=/item/4"},
		{"http://127.0.0.1:1/cb?state=s1&code=XYZ", "http://127.0.0.1:1/cb?state=s1&code=***"},
		{`Get "https://x/api?key=K1&access_token=A2": EOF`, `Get "https://x/api?key=***&access_token=***": EOF`},
		{"https://api/?Refresh_Token=r.t", "https://api/?Refresh_Token=***"},
		{"Authorization: Bearer abcdef0123456789", "Authorization: Bearer ***"},
		{"cookie: steamLoginSecure=abc; sessionid=1", "cookie: ***"},
		{"token exchange failed", "token exchange failed"},
		{"http listening url=http://127.0.0.1:5000", "http listening url=http://127.0.0.1:5000"},
		{"/item/1?tab=2", "/item/1?tab=2"},
	}
	for _, c := range cases {
		if got := String(c.in); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

type stringer string

func (s stringer) String() string { return string(s) }

func TestHandler(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(slog.NewTextHandler(&buf, nil))).With("base", "http://h/auth?t=w1")
	log.Info("open http://h/auth?t=m1", "url", "http://h/auth?t=s1",
		"err", errors.New(`Get "http://h/x?code=e1": EOF`), "u", stringer("http://h/?token=u1"),
		"refresh_token", "rt1", "n", 3, slog.Group("g", "url", "http://h/auth?t=g1"))
	out := buf.String()
	for _, secret := range []string{"w1", "m1", "s1", "e1", "u1", "rt1", "g1"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q leaked: %s", secret, out)
		}
	}
	if !strings.Contains(out, "auth?t=***") || !strings.Contains(out, "n=3") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.log")
	if err := File(p); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	in := "time=1 msg=ok\ntime=2 msg=\"browser open suppressed\" url=http://127.0.0.1:1/auth?t=deadbeef\n"
	if err := os.WriteFile(p, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := File(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p) //nolint:gosec // G304: test temp file
	want := "time=1 msg=ok\ntime=2 msg=\"browser open suppressed\" url=http://127.0.0.1:1/auth?t=***\n"
	if string(b) != want {
		t.Fatalf("got %q", b)
	}
}
