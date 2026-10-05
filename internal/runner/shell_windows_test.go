package runner

import (
	"strings"
	"testing"

	"github.com/UberMorgott/issuewatcher/internal/config"
)

// A verify / build command with double quotes reaches cmd.exe verbatim (Go's
// default argv escaping turns " into \" which cmd.exe does not understand).
func TestRunCommandQuotesReachCmdVerbatim(t *testing.T) {
	r := New(Options{Settings: config.Defaults, DataDir: t.TempDir()})
	dir := t.TempDir()
	res := r.RunCommand(t.Context(), dir, `echo "a b" && pwsh -NoProfile -NonInteractive -Command "$x = 'q r'; Write-Output ('got ' + $x)"`, dir, 0)
	if !res.OK || !strings.Contains(res.Output, `"a b"`) || strings.Contains(res.Output, `\"`) || !strings.Contains(res.Output, "got q r") {
		t.Fatalf("%+v", res)
	}
}
