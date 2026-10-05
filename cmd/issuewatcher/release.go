package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/release"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// newReleaseEngine wires autopilot release runs: GitHub releases and pushes
// with the user's token, the mod platforms' publishers, the runner's folder
// lock, verify gate and job-object commands; run / event changes go to the
// dashboard (SSE). srv is read at call time: it is set after the engine exists.
func newReleaseEngine(log *slog.Logger, dataDir string, cfgs *config.Store, st *store.Store, gh *github.Provider,
	jobs *runner.Runner, publishers func(string) provider.Publisher, srv func() *api.Server,
) *release.Engine {
	return release.New(release.Deps{
		Store: st, Settings: cfgs.Get, DataDir: dataDir, Releaser: gh, Publishers: publishers,
		DefaultBranch: gh.DefaultBranch, GitToken: gh.GitToken, TokenEnv: runner.TokenEnv, Folders: jobs,
		Gate: func(ctx context.Context, project, localPath, dir, logDir string) (release.CmdResult, bool) {
			v, ran := jobs.Gate(ctx, project, localPath, dir, logDir)
			return release.CmdResult(v), ran
		},
		Command: func(ctx context.Context, dir, command, logDir string, timeout time.Duration) release.CmdResult {
			return release.CmdResult(jobs.RunCommand(ctx, dir, command, logDir, timeout))
		},
		Log: log.With("component", "release"),
		OnChange: func(id int64) {
			if s := srv(); s != nil {
				s.RunChanged(id)
			}
		},
		OnEvent: func(ev store.AutopilotEvent) {
			if s := srv(); s != nil {
				s.AutopilotEvent(ev)
			}
		},
	})
}
