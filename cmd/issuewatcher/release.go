package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/release"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/smoke"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// newReleaseEngine wires autopilot release runs: GitHub releases and pushes
// with the user's token, the mod platforms' publishers, the runner's folder
// lock, verify gate and job-object commands; run / event changes go to the
// dashboard (SSE). srv is read at call time: it is set after the engine exists.
func newReleaseEngine(log *slog.Logger, dataDir string, cfgs *config.Store, settings *appSettings, st *store.Store, gh *github.Provider,
	jobs *runner.Runner, sy *syncer.Group, publishers func(string) provider.Publisher, srv func() *api.Server,
) *release.Engine {
	// Sync writes autopilot projects' new items / comments into the inbox (in
	// its own transaction); the kill switch stops that too.
	st.SetInboxFilter(func(key string) bool {
		a := cfgs.Get().Agents
		return !a.Autopilot.Paused && runner.AutopilotFixes(a, key)
	})
	return release.New(release.Deps{
		HasGate: jobs.HasGate, Fixer: jobs, Items: itemActions{st: st, sy: sy, gh: gh},
		Triager: jobs, Drafter: jobs,
		// The regression breaker switches the project's autopilot off (as the
		// panel's switch would) and tells the open tabs.
		PauseProject: func(_ context.Context, project string) error {
			patch, _ := json.Marshal(map[string]any{"agents": map[string]any{"projects": map[string]any{project: map[string]any{
				"autopilot": map[string]any{"enabled": false}}}}})
			var err error
			for range 3 { // a concurrent save moves the revision: read it again
				var doc api.SettingsDoc
				if doc, err = settings.PatchSettings(cfgs.Get().Revision, patch); err == nil {
					if s := srv(); s != nil {
						s.Publish(api.EventSettingsChanged, doc)
					}
					return nil
				}
			}
			return err
		},
		Collaborator: func(ctx context.Context, itemID int64) (bool, error) {
			ref, err := st.ItemRef(ctx, itemID)
			if err != nil || ref.Platform != store.CodePlatform {
				return false, err
			}
			a, err := gh.IssueAuthorAssociation(ctx, ref.ExternalID)
			return github.Collaborator(a), err
		},
		Store: st, Settings: cfgs.Get, DataDir: dataDir, Releaser: gh, Publishers: publishers,
		DefaultBranch: gh.DefaultBranch, GitToken: gh.GitToken, TokenEnv: runner.TokenEnv, Folders: jobs,
		Gate: func(ctx context.Context, project, localPath, dir, logDir string) (release.CmdResult, bool) {
			v, ran := jobs.Gate(ctx, project, localPath, dir, logDir)
			return release.CmdResult(v), ran
		},
		Command: func(ctx context.Context, dir, command, logDir string, timeout time.Duration) release.CmdResult {
			return release.CmdResult(jobs.RunCommand(ctx, dir, command, logDir, timeout))
		},
		Smoke: smoke.Runner{
			Exec: func(ctx context.Context, dir string, argv []string, logDir string, timeout time.Duration) smoke.CmdResult {
				return smoke.CmdResult(jobs.RunArgv(ctx, dir, argv, logDir, timeout))
			},
			Shell: func(ctx context.Context, dir, command, logDir string, timeout time.Duration) smoke.CmdResult {
				return smoke.CmdResult(jobs.RunCommand(ctx, dir, command, logDir, timeout))
			},
		}.Run,
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

// itemActions are the release run's reply / close actions on items: replies
// through the syncers (read-back included), GitHub issues closed and read
// through the GitHub provider.
type itemActions struct {
	st *store.Store
	sy *syncer.Group
	gh *github.Provider
}

func (a itemActions) CanReply(ctx context.Context, itemID int64) bool {
	ref, err := a.st.ItemRef(ctx, itemID)
	return err == nil && a.sy.CanReply(ref.Platform)
}

func (a itemActions) Reply(ctx context.Context, itemID int64, body string) (string, error) {
	c, err := a.sy.Reply(ctx, itemID, body)
	if err != nil {
		return "", err
	}
	return cmp.Or(c.URL, c.ExternalID), nil
}

// FindReply: GitHub reads the issue's comments live; other platforms only
// have the stored comments, so not finding it there proves nothing (error).
func (a itemActions) FindReply(ctx context.Context, itemID int64, marker string) (string, bool, error) {
	ref, err := a.st.ItemRef(ctx, itemID)
	if err != nil {
		return "", false, err
	}
	if ref.Platform == store.CodePlatform {
		_, comments, err := a.gh.IssueStatus(ctx, ref.ExternalID)
		if err != nil {
			return "", false, err
		}
		for _, c := range comments {
			if c.Author == ref.Account && strings.Contains(c.Body, marker) {
				return cmp.Or(c.URL, c.ExternalID), true, nil
			}
		}
		return "", false, nil
	}
	cursor := ""
	for {
		chunk, err := a.st.Comments(ctx, itemID, cursor, 200)
		if err != nil {
			return "", false, err
		}
		for _, c := range chunk.Items {
			if c.Author == ref.Account && strings.Contains(c.Body, marker) {
				return cmp.Or(c.URL, c.ExternalID), true, nil
			}
		}
		if !chunk.More {
			break
		}
		cursor = chunk.NextCursor
	}
	return "", false, errors.New(ref.Platform + ": no read-back of replies here, and the reply is not among the synced comments: check the item and skip or resume")
}

func (a itemActions) CloseItem(ctx context.Context, itemID int64) error {
	ref, err := a.st.ItemRef(ctx, itemID)
	if err != nil {
		return err
	}
	if ref.Platform != store.CodePlatform {
		return errors.New(ref.Platform + " items are not closed by autopilot")
	}
	return a.gh.CloseIssue(ctx, ref.ExternalID)
}

func (a itemActions) ItemClosed(ctx context.Context, itemID int64) (bool, error) {
	ref, err := a.st.ItemRef(ctx, itemID)
	if err != nil {
		return false, err
	}
	if ref.Platform != store.CodePlatform {
		return false, errors.New(ref.Platform + " items have no state autopilot reads")
	}
	open, _, err := a.gh.IssueStatus(ctx, ref.ExternalID)
	return !open, err
}
