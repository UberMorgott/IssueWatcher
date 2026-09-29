package mcpbridge

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Server stderr lands in the app log line by line: sign-in steps at info,
// failures at warn, the rest at debug; split writes are joined.
func TestStderrLog(t *testing.T) {
	var out bytes.Buffer
	w := &stderrLog{log: slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})), server: "nexus"}
	_, _ = w.Write([]byte("[login] extract: chrome 0 coo"))
	_, _ = w.Write([]byte("kies\n[browser-client] launch failed: x\nidle\n"))
	got := out.String()
	for _, want := range []string{
		`level=INFO msg="mcp: [login] extract: chrome 0 cookies" server=nexus`,
		`level=WARN msg="mcp: [browser-client] launch failed: x"`,
		`level=DEBUG msg="mcp: idle"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
}
