package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/issuewatcher/internal/control"
)

// releaseWaitTimeout bounds `release run --wait` (the run goes on in the app).
const releaseWaitTimeout = 2 * time.Hour

// project resolves a positional project (id, key or name).
func (c *cli) project(cmd string, pos []string) (int64, error) {
	if len(pos) != 1 || pos[0] == "" {
		return 0, usagef("%s: want one project (id, key or name)", cmd)
	}
	return c.c.ResolveProject(c.ctx, pos[0])
}

// splitList splits a comma list, dropping empty parts.
func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// release: `release plan|run <project> [--version --head --items --targets --dry-run --wait]`.
func (c *cli) release(args []string) (json.RawMessage, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub != "plan" && sub != "run" {
		return nil, usagef("release: want plan or run")
	}
	cmd := "release " + sub
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var req control.ReleaseRequest
	fs.StringVar(&req.Version, "version", "", "explicit version")
	fs.StringVar(&req.Head, "head", "", "commit sha to release")
	items := fs.String("items", "", "comma-separated item ids")
	targets := fs.String("targets", "", "comma-separated target keys")
	var wait *bool
	if sub == "run" {
		fs.BoolVar(&req.DryRun, "dry-run", false, "plan only")
		wait = fs.Bool("wait", false, "wait until the run is done, cancelled, failed or held")
	}
	pos, err := flags(fs, args[1:])
	if err != nil {
		return nil, err
	}
	for _, s := range splitList(*items) {
		id, err := parseID(s)
		if err != nil {
			return nil, err
		}
		req.Items = append(req.Items, id)
	}
	req.Targets = splitList(*targets)
	id, err := c.project(cmd, pos)
	if err != nil {
		return nil, err
	}
	if sub == "plan" {
		return c.c.ReleasePlan(c.ctx, id, req)
	}
	out, err := c.c.Release(c.ctx, id, req)
	if err != nil || req.DryRun || !*wait {
		return out, err
	}
	var started struct {
		Run struct {
			ID int64 `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(out, &started); err != nil || started.Run.ID == 0 {
		return out, fmt.Errorf("release: no run id in the answer: %s", out)
	}
	ctx, cancel := context.WithTimeout(c.ctx, releaseWaitTimeout)
	defer cancel()
	return c.c.RunWait(ctx, started.Run.ID)
}

// runs: `runs [--project P] [--state S] [--kind K] [--limit N]`.
func (c *cli) runs(args []string) (json.RawMessage, error) {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	var q control.RunQuery
	project := fs.String("project", "", "project id, key or name")
	fs.StringVar(&q.State, "state", "", "pending|running|held|done|cancelled|failed")
	fs.StringVar(&q.Kind, "kind", "", "run kind (release)")
	fs.IntVar(&q.Limit, "limit", 0, "newest runs (1-500)")
	pos, err := flags(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usagef("runs: unexpected %q", pos[0])
	}
	if *project != "" {
		if q.Project, err = c.c.ResolveProject(c.ctx, *project); err != nil {
			return nil, err
		}
	}
	return c.c.Runs(c.ctx, q)
}

// run: `run <id> [resume|cancel|skip <step> [--target T]]`.
func (c *cli) runCmd(args []string) (json.RawMessage, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	target := fs.String("target", "", "target key of a skipped step")
	pos, err := flags(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) == 0 {
		return nil, usagef("run: want a run id")
	}
	id, err := parseID(pos[0])
	if err != nil {
		return nil, err
	}
	switch {
	case len(pos) == 1 && *target == "":
		return c.c.Run(c.ctx, id)
	case len(pos) == 2 && slices.Contains(control.RunActions, pos[1]) && *target == "":
		return c.c.RunAction(c.ctx, id, pos[1])
	case len(pos) == 3 && pos[1] == "skip" && pos[2] != "":
		return c.c.SkipStep(c.ctx, id, pos[2], *target)
	}
	return nil, usagef("run: want <id> [resume|cancel|skip <step> [--target T]]")
}

// profile: `profile get <project>` · `profile set <project> (--file FILE | -)`.
func (c *cli) profile(args []string) (json.RawMessage, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "get":
		pos, err := flags(flag.NewFlagSet("profile get", flag.ContinueOnError), args[1:])
		if err != nil {
			return nil, err
		}
		id, err := c.project("profile get", pos)
		if err != nil {
			return nil, err
		}
		return c.c.PublishProfile(c.ctx, id)
	case "set":
		fs := flag.NewFlagSet("profile set", flag.ContinueOnError)
		file := fs.String("file", "", "JSON file {revision?, publishProfile?, autopilot?}")
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		stdin := len(pos) == 2 && pos[1] == "-"
		if stdin {
			pos = pos[:1]
		}
		if stdin == (*file != "") {
			return nil, usagef("profile set: give the JSON with --file FILE or - (stdin)")
		}
		var b []byte
		if stdin {
			b, err = io.ReadAll(io.LimitReader(c.stdin, 256<<10))
		} else {
			b, err = os.ReadFile(*file)
		}
		if err != nil {
			return nil, fmt.Errorf("read profile: %w", err)
		}
		b = trimBOM(b)
		if !json.Valid(b) {
			return nil, usagef("profile set: the profile is not valid JSON")
		}
		id, err := c.project("profile set", pos)
		if err != nil {
			return nil, err
		}
		return c.c.SetPublishProfile(c.ctx, id, b)
	}
	return nil, usagef("profile: want get or set")
}

// trimBOM drops a UTF-8 byte order mark (PowerShell's Set-Content writes one in 5.1).
func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}
