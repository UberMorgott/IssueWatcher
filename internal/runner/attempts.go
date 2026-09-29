package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Attempt is one run of a job: a finished attempt snapshotted by Retry, or the
// current one (docs/ARCHITECTURE.md → Automation, attempt history).
type Attempt struct {
	Attempt    int             `json:"attempt"`
	State      string          `json:"state"`
	Error      string          `json:"error"`
	ErrorCode  string          `json:"errorCode"`
	Result     json.RawMessage `json:"result"`
	StartedAt  string          `json:"startedAt"`
	FinishedAt string          `json:"finishedAt"`
}

func attemptPath(dataDir string, id int64, attempt int) string {
	return filepath.Join(jobDir(dataDir, id), strconv.Itoa(attempt)+".json")
}

func attemptOf(j store.Job) Attempt {
	return Attempt{Attempt: j.Attempt, State: j.State, Error: j.Error, ErrorCode: parseResult(j).ErrorCode,
		Result: j.Result, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt}
}

// writeAttempt snapshots the job's current attempt next to its log and diff.
func writeAttempt(dataDir string, j store.Job) error {
	b, err := json.Marshal(attemptOf(j))
	if err != nil {
		return fmt.Errorf("runner: encode attempt: %w", err)
	}
	if err := os.MkdirAll(jobDir(dataDir, j.ID), 0o750); err != nil {
		return fmt.Errorf("runner: attempt dir: %w", err)
	}
	if err := os.WriteFile(attemptPath(dataDir, j.ID, j.Attempt), b, 0o600); err != nil {
		return fmt.Errorf("runner: write attempt: %w", err)
	}
	return nil
}

// Attempts lists job id's earlier attempts that have a snapshot (jobs retried
// before snapshots existed have none), oldest first, then the current attempt.
func (r *Runner) Attempts(ctx context.Context, id int64) ([]Attempt, error) {
	j, err := r.opts.Store.Job(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]Attempt, 0, j.Attempt)
	for n := 1; n < j.Attempt; n++ {
		b, err := os.ReadFile(attemptPath(r.opts.DataDir, id, n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("runner: read attempt: %w", err)
		}
		var a Attempt
		if err := json.Unmarshal(b, &a); err != nil || a.Attempt != n {
			continue // a damaged snapshot is skipped, not fatal
		}
		out = append(out, a)
	}
	return append(out, attemptOf(j)), nil
}
