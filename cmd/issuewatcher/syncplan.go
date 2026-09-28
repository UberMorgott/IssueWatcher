package main

import (
	"time"

	"github.com/UberMorgott/issuewatcher/internal/config"
	"github.com/UberMorgott/issuewatcher/internal/syncer"
)

// syncPlan maps Settings → Sync to the GitHub polling plan.
func syncPlan(s config.Sync) syncer.Plan {
	p := s.Plan("github")
	return syncer.Plan{
		Mode: s.Mode, Active: p.Active(), Idle: p.Idle(), Reconcile: p.Reconcile(),
		ActiveWindow: time.Duration(s.ActiveDays) * 24 * time.Hour,
		Budget:       p.HourlyBudget, Concurrency: p.Concurrency,
	}
}
