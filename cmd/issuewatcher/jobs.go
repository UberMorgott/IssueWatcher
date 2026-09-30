package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
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
func newRunner(log *slog.Logger, dataDir string, cfgs *config.Store, st *store.Store, gh *github.Provider, sy *syncer.Group,
	srv func() *api.Server, tray func() *notify.Tray,
) *runner.Runner {
	exe, err := os.Executable() // the job's MCP server: <exe> mcp --item <id>
	if err != nil {
		log.Warn("runner: no executable path, jobs run without the MCP server", "err", err)
	}
	return runner.New(runner.Options{
		Store: st, Settings: cfgs.Get, DataDir: dataDir, Publisher: gh, Reply: sy.Reply, ReplyThreaded: sy.ReplyThreaded, MaxReply: sy.MaxReply, Labels: gh, Log: log, Exe: exe,
		OnJob: func(j store.Job) {
			s := srv()
			if s == nil {
				return
			}
			s.Publish(api.EventJobChanged, j)
			if j.ItemID != 0 && (j.Phase == "" || j.Phase == "prepare") { // state changes only: issue rows show the job badge (a triage has no issue)
				s.Publish(api.EventDataChanged, api.DataChange{Reason: "job", ItemID: j.ItemID})
			}
		},
		OnSteps: func(id int64, attempt int, steps []runner.Step) {
			if s := srv(); s != nil {
				s.Publish(api.EventJobLog, api.JobSteps{ID: id, Attempt: attempt, Steps: steps})
			}
		},
		OnFinished: func(j store.Job) {
			if j.ItemID == 0 { // a project triage: its fix jobs notify when they finish; the dashboard shows the ranking
				log.Info("agent job finished", "job", j.ID, "flow", j.Flow, "state", j.State)
				return
			}
			ok := j.State == store.JobNeedsReview || j.State == store.JobDone // done: a rule label job applied its labels
			var res runner.Result
			_ = json.Unmarshal(j.Result, &res)
			text := notify.FailReason(res.ErrorCode, j.Error)
			if ok {
				switch {
				case j.Flow == "label":
					text = strings.Join(res.Labels, ", ")
				case res.Agent != nil && res.Agent.Summary != "":
					text = res.Agent.Summary
				case res.Draft != "":
					text = res.Draft
				}
			}
			t := tray()
			shown := t != nil && t.NotifyCard(notify.JobCard(j.ItemID, j.Repo, j.Number, ok, outcomeLabel(j, res), text, time.Now()))
			log.Info("agent job finished", "job", j.ID, "state", j.State, "card", shown)
		},
	})
}

// outcomeLabel is the short Russian outcome on the «Агент закончил» card.
func outcomeLabel(j store.Job, res runner.Result) string {
	if j.Flow == "label" && j.State == store.JobDone {
		return "метки добавлены"
	}
	if j.State != store.JobNeedsReview {
		return "ошибка"
	}
	if j.Flow == "label" {
		return "метки предложены"
	}
	if res.Local != nil {
		return map[string]string{
			runner.OutcomeFixedLocal: "исправлено локально", runner.OutcomePushed: "отправлено",
			runner.OutcomeNotReproduced: "не воспроизводится", runner.OutcomeNeedsInfo: "нужна информация",
			runner.OutcomeNoCommit: "коммита нет", runner.OutcomeFailed: "ошибка",
		}[res.Local.Outcome]
	}
	if j.Flow == "reply" {
		return "черновик ответа"
	}
	return "готово к проверке"
}
