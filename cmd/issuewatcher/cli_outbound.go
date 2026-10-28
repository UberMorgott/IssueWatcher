package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/UberMorgott/issuewatcher/internal/control"
)

// Exit codes of the URL-addressed verbs on top of exitOK / exitAPI / exitUsage /
// exitNotRunning (docs/AGENT-CLI.md → Exit codes).
const (
	exitRefused        = 4 // refused before anything was sent (4xx: unsupported, gate closed, agent caller, too long…)
	exitOutcomeUnknown = 5 // sent once, the result is unknown: check the page, never resend blindly
)

// isOutbound: post / read / download, and reply addressed with --url (reply
// <itemId> stays the synced-item reply).
func isOutbound(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "post", "read", "download":
		return true
	case "reply":
		return slices.ContainsFunc(args[1:], func(a string) bool {
			return a == "--url" || a == "-url" || strings.HasPrefix(a, "--url=") || strings.HasPrefix(a, "-url=")
		})
	}
	return false
}

// runOutbound runs `post|reply|read|download --url U …` and returns the exit code.
// --json prints one compact JSON line on stdout, also for errors
// ({ok:false, code, error, status}).
func runOutbound(c *cli, args []string, stdout, stderr io.Writer) int {
	verb := args[0]
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	target := fs.String("url", "", "target URL")
	parent := fs.String("parent", "", "reply: the comment answered (id or URL)")
	file := fs.String("body-file", "", "file with the comment body")
	out := fs.String("out", "", "download: output file")
	dry := fs.Bool("dry-run", false, "plan only, post nothing")
	compact := fs.Bool("json", false, "one compact JSON line on stdout, errors included")
	pos, err := flags(fs, args[1:])
	if err == nil {
		err = checkOutboundFlags(verb, fs, *target, *file, *out, pos)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "issuewatcher:", strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
		_, _ = fmt.Fprintln(stderr, "usage: issuewatcher-cli post|reply|read|download --url URL [--parent P] [--body-file F | -] [--out F] [--dry-run] [--json] (docs/AGENT-CLI.md)")
		return exitUsage
	}
	req := control.OutboundRequest{URL: *target, Parent: *parent, DryRun: *dry}
	if verb == "post" || verb == "reply" {
		var b []byte
		if len(pos) == 1 {
			b, err = io.ReadAll(io.LimitReader(c.stdin, 1<<20))
		} else {
			b, err = os.ReadFile(*file)
		}
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "issuewatcher: read body:", err)
			return exitUsage
		}
		req.Body = string(trimBOM(b))
	}
	var res json.RawMessage
	if verb == "download" {
		res, err = c.c.Download(c.ctx, req, *out)
	} else {
		res, err = c.c.Outbound(c.ctx, verb, req)
	}
	if err != nil {
		return outboundFail(err, verb, *compact, stdout, stderr)
	}
	if *compact {
		var buf strings.Builder
		if cerr := compactJSON(&buf, res); cerr != nil {
			_, _ = fmt.Fprintln(stderr, "issuewatcher:", cerr)
			return exitAPI
		}
		_, _ = io.WriteString(stdout, buf.String()+"\n")
		return exitOK
	}
	if err := writeJSONOut(stdout, res); err != nil {
		_, _ = fmt.Fprintln(stderr, "issuewatcher:", err)
		return exitAPI
	}
	return exitOK
}

func checkOutboundFlags(verb string, fs *flag.FlagSet, target, file, out string, pos []string) error {
	if target == "" {
		return usagef("%s: want --url", verb)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	write := verb == "post" || verb == "reply"
	stdin := len(pos) == 1 && pos[0] == "-"
	switch {
	case write && ((len(pos) > 0 && !stdin) || stdin == (file != "")):
		return usagef("%s: give the body with --body-file FILE or - (stdin)", verb)
	case !write && (len(pos) > 0 || set["body-file"] || set["dry-run"]):
		return usagef("%s: takes no body and no --dry-run (it never writes to the platform)", verb)
	case verb == "post" && set["parent"]:
		return usagef("post: no --parent (a new top-level comment); use reply")
	case verb == "download" && out == "":
		return usagef("download: want --out FILE")
	case verb != "download" && set["out"]:
		return usagef("%s: --out is for download", verb)
	}
	return nil
}

// outboundFail prints err and returns its exit code.
func outboundFail(err error, verb string, compact bool, stdout, stderr io.Writer) int {
	code, exit, status := "", exitAPI, 0
	var apiErr *control.APIError
	switch {
	case errors.Is(err, control.ErrNotRunning):
		code, exit = "not_running", exitNotRunning
	case errors.As(err, &apiErr):
		code, status = apiErr.Code, apiErr.Status
		switch {
		case code == "outcome_unknown":
			exit = exitOutcomeUnknown
		case status >= 400 && status < 500 && status != http.StatusTooManyRequests:
			exit = exitRefused
		}
	}
	if compact {
		msg := err.Error()
		if apiErr != nil {
			msg = apiErr.Message
		}
		b, _ := json.Marshal(map[string]any{"ok": false, "code": cmp.Or(code, "error"), "verb": verb, "error": msg, "status": status, "exit": exit})
		_, _ = stdout.Write(append(b, '\n'))
	}
	_, _ = fmt.Fprintln(stderr, "issuewatcher:", err)
	return exit
}

func compactJSON(w io.StringWriter, doc json.RawMessage) error {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return fmt.Errorf("bad JSON from the app: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.WriteString(string(b))
	return err
}
