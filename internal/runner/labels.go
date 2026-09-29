package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/provider"
	"github.com/UberMorgott/issuewatcher/internal/store"
)

// Label flow (docs/ARCHITECTURE.md → Automation): a read-only agent picks
// from the repository's existing labels; «Добавить метки» (ApplyLabels) adds
// the chosen ones, never removing any. A rule job of a project with
// autoApplyLabels on adds its picks itself.

// maxApplyLabels bounds one ApplyLabels call.
const maxApplyLabels = 50

// RepoLabels returns the labels of project (owner/repo).
func (r *Runner) RepoLabels(ctx context.Context, project string) ([]provider.Label, error) {
	if r.opts.Labels == nil {
		return nil, ErrUnavailable
	}
	return r.opts.Labels.ListLabels(ctx, project)
}

// matchLabels splits names into the repository's labels (canonical spelling,
// matched case-insensitively as GitHub does, deduplicated) and unknown ones.
func matchLabels(repo []provider.Label, names []string) (known, unknown []string) {
	canon := make(map[string]string, len(repo))
	for _, l := range repo {
		canon[strings.ToLower(l.Name)] = l.Name
	}
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		k := strings.ToLower(n)
		if n == "" || seen[k] {
			continue
		}
		seen[k] = true
		if c, ok := canon[k]; ok {
			known = append(known, c)
		} else {
			unknown = append(unknown, n)
		}
	}
	return known, unknown
}

func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// addLabels re-checks names against the repository's labels (any unknown →
// ErrBadRequest), adds those the item lacks and stores the item's labels.
// It returns the names added (none when all were already there).
func (r *Runner) addLabels(ctx context.Context, itemID int64, names []string) ([]string, error) {
	in, err := r.opts.Store.JobInput(ctx, itemID)
	if err != nil {
		return nil, err
	}
	repo, err := r.opts.Labels.ListLabels(ctx, in.ProjectName)
	if err != nil {
		return nil, err
	}
	known, unknown := matchLabels(repo, names)
	if len(unknown) > 0 {
		return nil, fmt.Errorf("%w: %s has no label %s", ErrBadRequest, in.ProjectName, strings.Join(unknown, ", "))
	}
	var add []string
	for _, n := range known {
		if !hasLabel(in.Labels, n) {
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return []string{}, nil
	}
	labels, err := r.opts.Labels.AddLabels(ctx, in.ProjectName, in.Number, add)
	if err != nil {
		return nil, err
	}
	return add, r.opts.Store.SetItemLabels(ctx, itemID, labels)
}

// runLabel: the responder picks labels, read-only; picks outside the
// repository's list are dropped.
func (r *Runner) runLabel(ctx context.Context, j *store.Job, res *Result, log *jobLog) (string, error) {
	if r.opts.Labels == nil {
		return "", ErrUnavailable
	}
	prof, cfg, err := r.profileFor(*j)
	if err != nil {
		return "", err
	}
	in, err := r.opts.Store.JobInput(ctx, j.ItemID)
	if err != nil {
		return "", err
	}
	if _, err := r.resolveExe(prof); err != nil {
		return "", coded(CodeNoCLI, err)
	}
	repo, err := r.opts.Labels.ListLabels(ctx, in.ProjectName)
	if err != nil {
		return "", fmt.Errorf("list the labels of %s: %w", in.ProjectName, err)
	}
	if len(repo) == 0 {
		return "", fmt.Errorf("%s has no labels to pick from", in.ProjectName)
	}
	log.addf(StepInfo, "%s has %d labels", in.ProjectName, len(repo))
	files, dir, err := r.readOnlyDir(*j, in, log)
	if err != nil {
		return "", err
	}
	r.phase(ctx, j, "agent")
	system, task := prompts(cfg, flowLabel, promptInput{in: in, labels: repo})
	agent, err := r.runAgent(ctx, agentSpec{profile: prof, flow: flowLabel, dir: dir, workDir: files, system: system, task: task, readOnly: true}, log)
	res.Agent = &agent
	if err != nil {
		if errors.Is(err, errTimeout) || errors.Is(err, ErrCancelled) {
			return "", err
		}
		return "", coded(CodeAgent, err)
	}
	res.Labels, res.DroppedLabels = matchLabels(repo, agent.Labels)
	if res.Labels == nil {
		res.Labels = []string{}
	}
	log.addf(StepInfo, "suggested labels: %s", listOrNone(res.Labels))
	if len(res.DroppedLabels) > 0 {
		log.addf(StepInfo, "dropped (not in the repository): %s", strings.Join(res.DroppedLabels, ", "))
	}
	if j.Origin == store.OriginRule && len(res.Labels) > 0 && cfg.AutomationFor(in.ProjectName).AutoApplyLabels {
		return r.autoApply(ctx, j.ItemID, res, log), nil
	}
	return store.JobNeedsReview, nil
}

// autoApply adds a rule job's picks itself: done, or needs_review with the
// publish error when adding failed (the user can still apply them).
func (r *Runner) autoApply(ctx context.Context, itemID int64, res *Result, log *jobLog) string {
	added, err := r.addLabels(ctx, itemID, res.Labels)
	if err != nil {
		res.PublishError = err.Error()
		log.add(StepError, "auto-apply labels: "+err.Error())
		return store.JobNeedsReview
	}
	res.AppliedLabels = added
	log.addf(StepInfo, "labels added automatically: %s", listOrNone(added))
	return store.JobDone
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// ApplyLabels («Добавить метки») adds names to the label job's issue: every
// name must exist in the repository (re-checked now); labels already on the
// issue stay and nothing is removed. The job ends done.
func (r *Runner) ApplyLabels(ctx context.Context, id int64, names []string) (store.Job, error) {
	if r.opts.Labels == nil {
		return store.Job{}, ErrUnavailable
	}
	var clean []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			clean = append(clean, n)
		}
	}
	if len(clean) == 0 || len(clean) > maxApplyLabels {
		return store.Job{}, fmt.Errorf("%w: 1–%d labels", ErrBadRequest, maxApplyLabels)
	}
	j, err := r.lockPublish(ctx, id, flowLabel)
	if err != nil {
		return j, err
	}
	res := parseResult(j)
	log, lerr := openAppendLog(r.opts.DataDir, j.ID, j.Attempt, func(s Step) { r.opts.OnSteps(j.ID, j.Attempt, []Step{s}) })
	if lerr != nil {
		return r.unlockPublish(ctx, j, res, lerr)
	}
	defer log.close()
	added, perr := r.addLabels(ctx, j.ItemID, clean)
	if perr != nil {
		log.add(StepError, "add labels: "+perr.Error())
	} else {
		res.AppliedLabels = added
		log.addf(StepInfo, "labels added: %s", listOrNone(added))
	}
	return r.unlockPublish(ctx, j, res, perr)
}
