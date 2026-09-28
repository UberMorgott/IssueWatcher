package runner

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
)

// Log returns the steps of attempt of job id (0 = the current attempt).
func (r *Runner) Log(id int64, attempt int) ([]Step, error) {
	return ReadLog(r.opts.DataDir, id, attempt)
}

// Diff returns the stored diff of attempt of job id.
func (r *Runner) Diff(id int64, attempt int) (string, error) {
	return ReadDiff(r.opts.DataDir, id, attempt)
}

// Detected is one CLI found on PATH.
type Detected struct {
	CLI     string `json:"cli"`
	Path    string `json:"path"`    // "" = not found
	Version string `json:"version"` // first line of --version
}

// Detect looks for the claude and codex CLIs on PATH (Settings › Agents).
func (r *Runner) Detect(ctx context.Context) []Detected {
	var out []Detected
	for _, cli := range []string{config.CLIClaude, config.CLICodex} {
		d := Detected{CLI: cli}
		if p, err := r.opts.LookPath(cli); err == nil {
			d.Path = p
			if cli == config.CLICodex {
				if n := nativeCodex(p); n != "" {
					d.Path = n
				}
			}
			d.Version = version(ctx, d.Path)
		}
		out = append(out, d)
	}
	return out
}

func version(ctx context.Context, exe string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	cmd.Env = agentEnv()
	prepare(cmd)
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(line)
}
