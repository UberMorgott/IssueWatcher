package api

import (
	"context"
	"sync"

	"github.com/UberMorgott/issuewatcher/internal/runner"
)

// agentDetect caches the agent CLI detection (claude/codex on PATH and their
// versions): running the CLIs costs tens of milliseconds, so it runs once at
// start in the background, again after a settings change or on
// GET /api/agents/detect?refresh=1; plain GETs answer from the cache.
type agentDetect struct {
	mu  sync.Mutex // held while detecting: a GET waits for a running detection
	res []runner.Detected
	ok  bool
}

// detectAgents returns the cached detection, detecting first when there is
// none yet or refresh is set.
func (s *Server) detectAgents(ctx context.Context, refresh bool) []runner.Detected {
	d := &s.agents
	d.mu.Lock()
	defer d.mu.Unlock()
	if refresh || !d.ok {
		d.res, d.ok = s.opts.Runner.Detect(ctx), ctx.Err() == nil
	}
	return d.res
}

// redetectAgents drops the cache and detects again in the background.
func (s *Server) redetectAgents() {
	if s.opts.Runner == nil {
		return
	}
	s.agents.mu.Lock()
	s.agents.ok = false
	s.agents.mu.Unlock()
	go s.detectAgents(s.bg, false)
}
