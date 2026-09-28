package main

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/api"
	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/notify"
	"github.com/UberMorgott/issuewatcher/internal/provider/github"
	"github.com/UberMorgott/issuewatcher/internal/runner"
	"github.com/UberMorgott/issuewatcher/internal/store"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// newRunner wires the agent job queue to the dashboard (SSE job.changed /
// job.log, data.changed for issue badges) and the tray («Агент закончил»).
// srv and tray are read at call time: both are set after the runner exists.
func newRunner(log *slog.Logger, dataDir string, cfgs *config.Store, st *store.Store, gh *github.Provider, sy *syncer.Syncer,
	srv func() *api.Server, tray func() *notify.Tray,
) *runner.Runner {
	return runner.New(runner.Options{
		Store: st, Settings: cfgs.Get, DataDir: dataDir, Publisher: gh, Reply: sy.Reply, Log: log,
		OnJob: func(j store.Job) {
			s := srv()
			if s == nil {
				return
			}
			s.Publish(api.EventJobChanged, j)
			if j.Phase == "" || j.Phase == "prepare" { // state changes only: issue rows show the job badge
				s.Publish(api.EventDataChanged, api.DataChange{Reason: "job", ItemID: j.ItemID})
			}
		},
		OnSteps: func(id int64, attempt int, steps []runner.Step) {
			if s := srv(); s != nil {
				s.Publish(api.EventJobLog, api.JobSteps{ID: id, Attempt: attempt, Steps: steps})
			}
		},
		OnFinished: func(j store.Job) {
			ok := j.State == store.JobNeedsReview
			text := j.Error
			if ok {
				var res runner.Result
				_ = json.Unmarshal(j.Result, &res)
				switch {
				case res.Agent != nil && res.Agent.Summary != "":
					text = res.Agent.Summary
				case res.Draft != "":
					text = res.Draft
				}
			}
			t := tray()
			shown := t != nil && t.NotifyCard(notify.JobCard(j.ID, j.Repo, j.Number, ok, text, time.Now()))
			log.Info("agent job finished", "job", j.ID, "state", j.State, "card", shown)
		},
	})
}
