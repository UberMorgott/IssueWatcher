package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/control"
	"github.com/UberMorgott/issuewatcher/internal/paths"
)

// CLI exit codes (docs/ARCHITECTURE.md → Control).
const (
	exitOK         = 0
	exitAPI        = 1
	exitUsage      = 2
	exitNotRunning = 3
)

// errUsage marks a command-line mistake (exit 2).
var errUsage = errors.New("usage")

const cliUsage = `usage: issuewatcher <command> [flags]

Talks to the running IssueWatcher of this data dir (never starts it).
JSON on stdout, errors on stderr; exit 0 ok, 1 API error, 2 usage, 3 not running.

  help

  status
  projects
  items [--project ID] [--state open|closed] [--label L] [--q TEXT] [--unread] [--limit N] [--cursor C]
  item <id> [--comments N]
  jobs [--state S] [--flow F] [--origin manual|rule] [--project ID] [--item ID] [--limit N] [--cursor C]
  job <id>
  job log <id> [--attempt N]
  sync
  reply <itemId> (--body-file FILE | -)
  jobs create --flow fix|reply|label [--profile ID] <itemId>...
  job cancel|retry|dismiss|push|pr <id>
  job reply <id> (--body-file FILE | -)
  job labels <id> <name>...
`

// isCLI reports whether args start with a subcommand (a word, not a flag):
// such a launch is a CLI call and never becomes the desktop app.
func isCLI(args []string) bool {
	return len(args) > 0 && args[0] != "" && !strings.HasPrefix(args[0], "-") && !strings.HasPrefix(args[0], "/")
}

// runCLI runs one subcommand and returns the process exit code. It runs before
// the single-instance lock: no tray, DB or log file of its own.
func runCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(code int, err error) int {
		_, _ = fmt.Fprintln(stderr, "issuewatcher:", err)
		return code
	}
	if len(args) > 0 && args[0] == "help" {
		_, _ = io.WriteString(stdout, cliUsage)
		return exitOK
	}
	dataDir, err := paths.DataDir()
	if err != nil {
		return fail(exitAPI, err)
	}
	c := &cli{c: control.New(dataDir), stdin: stdin, ctx: context.Background()}
	out, err := c.run(args)
	var apiErr *control.APIError
	switch {
	case errors.Is(err, errUsage):
		if msg := strings.TrimPrefix(err.Error(), errUsage.Error()); msg != "" {
			_, _ = fmt.Fprintln(stderr, "issuewatcher"+msg)
		}
		_, _ = io.WriteString(stderr, cliUsage)
		return exitUsage
	case errors.Is(err, control.ErrNotRunning):
		return fail(exitNotRunning, err)
	case errors.As(err, &apiErr):
		return fail(exitAPI, err)
	case err != nil:
		return fail(exitAPI, err)
	}
	if err := writeJSONOut(stdout, out); err != nil {
		return fail(exitAPI, err)
	}
	return exitOK
}

// writeJSONOut prints a JSON document indented, one per call.
func writeJSONOut(w io.Writer, doc json.RawMessage) error {
	if len(doc) == 0 {
		doc = json.RawMessage(`{"ok":true}`)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, doc, "", "  "); err != nil {
		return fmt.Errorf("bad JSON from the app: %w", err)
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

type cli struct {
	c     *control.Client
	stdin io.Reader
	ctx   context.Context //nolint:containedctx // one CLI call
}

func usagef(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errUsage}, a...)...)
}

// flags parses fs over args with flags and positionals in any order.
func flags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usagef("%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, usagef("bad id %q", s)
	}
	return id, nil
}

// oneID expects exactly one positional id.
func oneID(cmd string, pos []string) (int64, error) {
	if len(pos) != 1 {
		return 0, usagef("%s: want one id", cmd)
	}
	return parseID(pos[0])
}

func (c *cli) run(args []string) (json.RawMessage, error) {
	if len(args) == 0 {
		return nil, errUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "status":
		if _, err := flags(flag.NewFlagSet(cmd, flag.ContinueOnError), rest); err != nil {
			return nil, err
		}
		return c.c.Status(c.ctx)
	case "projects":
		if _, err := flags(flag.NewFlagSet(cmd, flag.ContinueOnError), rest); err != nil {
			return nil, err
		}
		return c.c.Projects(c.ctx)
	case "sync":
		if _, err := flags(flag.NewFlagSet(cmd, flag.ContinueOnError), rest); err != nil {
			return nil, err
		}
		return nil, c.c.Sync(c.ctx)
	case "reply":
		id, body, err := c.idAndBody("reply", rest)
		if err != nil {
			return nil, err
		}
		return c.c.Reply(c.ctx, id, body)
	case "items":
		return c.items(rest)
	case "item":
		return c.item(rest)
	case "jobs":
		return c.jobs(rest)
	case "job":
		return c.job(rest)
	}
	return nil, usagef("unknown command %q", cmd)
}

func (c *cli) items(args []string) (json.RawMessage, error) {
	fs := flag.NewFlagSet("items", flag.ContinueOnError)
	var q control.ItemQuery
	var limit int
	fs.Int64Var(&q.Project, "project", 0, "project id")
	fs.StringVar(&q.State, "state", "", "open|closed")
	fs.StringVar(&q.Label, "label", "", "label")
	fs.StringVar(&q.Text, "q", "", "text search")
	fs.BoolVar(&q.Unread, "unread", false, "unread only")
	fs.IntVar(&limit, "limit", 0, "page size")
	fs.StringVar(&q.Cursor, "cursor", "", "next page cursor")
	pos, err := flags(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usagef("items: unexpected %q", pos[0])
	}
	q.Limit = limit
	return c.c.Items(c.ctx, q)
}

func (c *cli) item(args []string) (json.RawMessage, error) {
	fs := flag.NewFlagSet("item", flag.ContinueOnError)
	comments := fs.Int("comments", 100, "comments to include (oldest first)")
	pos, err := flags(fs, args)
	if err != nil {
		return nil, err
	}
	id, err := oneID("item", pos)
	if err != nil {
		return nil, err
	}
	return c.c.Item(c.ctx, id, *comments)
}

// idAndBody parses `<id> (--body-file f | -)`: the body from the file or stdin.
func (c *cli) idAndBody(cmd string, args []string) (int64, string, error) {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	file := fs.String("body-file", "", "file with the comment body")
	pos, err := flags(fs, args)
	if err != nil {
		return 0, "", err
	}
	stdin := len(pos) == 2 && pos[1] == "-"
	if stdin {
		pos = pos[:1]
	}
	if stdin == (*file != "") {
		return 0, "", usagef("%s: give the body with --body-file FILE or - (stdin)", cmd)
	}
	id, err := oneID(cmd, pos)
	if err != nil {
		return 0, "", err
	}
	var b []byte
	if stdin {
		b, err = io.ReadAll(io.LimitReader(c.stdin, 1<<20))
	} else {
		b, err = os.ReadFile(*file)
	}
	if err != nil {
		return 0, "", fmt.Errorf("read body: %w", err)
	}
	return id, string(b), nil
}

func (c *cli) jobsCreate(args []string) (json.RawMessage, error) {
	fs := flag.NewFlagSet("jobs create", flag.ContinueOnError)
	flow := fs.String("flow", "", "fix|reply|label")
	profile := fs.String("profile", "", "agent profile id (default: the flow's role)")
	pos, err := flags(fs, args)
	if err != nil {
		return nil, err
	}
	if *flow == "" || len(pos) == 0 {
		return nil, usagef("jobs create: want --flow and item ids")
	}
	ids := make([]int64, 0, len(pos))
	for _, p := range pos {
		id, err := parseID(p)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return c.c.CreateJobs(c.ctx, *flow, ids, *profile)
}

func (c *cli) jobs(args []string) (json.RawMessage, error) {
	if len(args) > 0 && args[0] == "create" {
		return c.jobsCreate(args[1:])
	}
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	var q control.JobQuery
	fs.StringVar(&q.State, "state", "", "queued|running|needs_review|done|failed|cancelled")
	fs.StringVar(&q.Flow, "flow", "", "fix|reply|label")
	fs.StringVar(&q.Origin, "origin", "", "manual|rule")
	fs.Int64Var(&q.Project, "project", 0, "project id")
	fs.Int64Var(&q.Item, "item", 0, "item id")
	fs.IntVar(&q.Limit, "limit", 0, "page size")
	fs.StringVar(&q.Cursor, "cursor", "", "next page cursor")
	pos, err := flags(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usagef("jobs: unexpected %q", pos[0])
	}
	return c.c.Jobs(c.ctx, q)
}

func (c *cli) job(args []string) (json.RawMessage, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case slices.Contains(control.JobActions, sub):
		pos, err := flags(flag.NewFlagSet("job "+sub, flag.ContinueOnError), args[1:])
		if err != nil {
			return nil, err
		}
		id, err := oneID("job "+sub, pos)
		if err != nil {
			return nil, err
		}
		return c.c.JobAction(c.ctx, id, sub)
	case sub == "reply":
		id, body, err := c.idAndBody("job reply", args[1:])
		if err != nil {
			return nil, err
		}
		return c.c.JobReply(c.ctx, id, body)
	case sub == "labels":
		pos, err := flags(flag.NewFlagSet("job labels", flag.ContinueOnError), args[1:])
		if err != nil {
			return nil, err
		}
		if len(pos) < 2 {
			return nil, usagef("job labels: want a job id and label names")
		}
		id, err := parseID(pos[0])
		if err != nil {
			return nil, err
		}
		return c.c.JobLabels(c.ctx, id, pos[1:])
	}
	if sub == "log" {
		fs := flag.NewFlagSet("job log", flag.ContinueOnError)
		attempt := fs.Int("attempt", 0, "attempt (default: current)")
		pos, err := flags(fs, args[1:])
		if err != nil {
			return nil, err
		}
		id, err := oneID("job log", pos)
		if err != nil {
			return nil, err
		}
		return c.c.JobLog(c.ctx, id, *attempt)
	}
	pos, err := flags(flag.NewFlagSet("job", flag.ContinueOnError), args)
	if err != nil {
		return nil, err
	}
	id, err := oneID("job", pos)
	if err != nil {
		return nil, err
	}
	return c.c.Job(c.ctx, id)
}

// cliMain runs a CLI launch (stdio attached first for the GUI build).
func cliMain(args []string) int {
	attachConsole()
	return runCLI(args, os.Stdin, os.Stdout, os.Stderr)
}
