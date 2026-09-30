// Command fakecli stands in for the claude and codex CLIs in runner tests. It
// speaks the same event formats (stream-json / --json), reads the prompt from
// stdin and acts by FAKECLI_MODE:
//
//	ok    edit fixed.txt (write flows), report a structured result, exit 0;
//	      the direct fix flow also commits it with "Fixes #N"; the folder fix
//	      flow (no git) also deletes obsolete.txt
//	nofixes  direct fix: commit without "Fixes #N"
//	spawn like ok, but first start a child that sleeps holding stdout (its pid
//	      goes to FAKECLI_RECORD.pid) and leave it running
//	noop  report "cannot_fix" (direct: "not_reproduced") without touching files, exit 0
//	fail  emit a step, exit 1
//	authfail  claude's expired sign-in: an "authentication_failed" assistant
//	      message and an is_error result of subtype "success", exit 1
//	hang  start a child that sleeps, write its pid to FAKECLI_RECORD.pid, sleep
//	sleep sleep (the child of hang)
//
// The label flow answers the comma-separated FAKECLI_LABELS as its picks. The
// triage flow answers FAKECLI_PICKS ("number:severity,..."), or else every
// "#N " line of its prompt in order, severity high.
// FAKECLI_RECORD (a file) receives {args, stdin, ghToken, mcpConfig} for assertions.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

func main() {
	mode := os.Getenv("FAKECLI_MODE")
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("fakecli 1.0")
		return
	}
	if mode == "sleep" {
		time.Sleep(10 * time.Minute)
		return
	}
	args := os.Args[1:]
	codex := len(args) > 0 && args[0] == "exec"
	stdin, _ := io.ReadAll(os.Stdin)
	if rec := os.Getenv("FAKECLI_RECORD"); rec != "" {
		var mcpConfig string // claude's per-run --mcp-config file, read while it exists
		if i := slices.Index(args, "--mcp-config"); i >= 0 && i+1 < len(args) {
			b, _ := os.ReadFile(args[i+1])
			mcpConfig = string(b)
		}
		b, _ := json.Marshal(map[string]any{"args": args, "stdin": string(stdin), "ghToken": os.Getenv("GH_TOKEN"), "mcpConfig": mcpConfig})
		_ = os.WriteFile(rec, b, 0o600)
	}
	flow := "fix"
	schema := strings.Join(args, " ")
	if i := slices.Index(args, "--output-schema"); i >= 0 && i+1 < len(args) {
		b, _ := os.ReadFile(args[i+1])
		schema = string(b)
	}
	switch {
	case strings.Contains(schema, `"picks"`):
		flow = "triage"
	case strings.Contains(schema, `"labels"`):
		flow = "label"
	case strings.Contains(schema, `"reply"`):
		flow = "reply"
	case strings.Contains(schema, `"verdict"`):
		flow = "review"
	case strings.Contains(schema, `"files"`):
		flow = "fix-folder"
	case strings.Contains(schema, `not_reproduced`):
		flow = "fix-direct"
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	if codex {
		emit(map[string]any{"type": "thread.started", "thread_id": "t-1"})
		emit(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": "working on " + flow}})
	} else {
		emit(map[string]any{"type": "system", "subtype": "init", "model": "fake-model"})
		emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "working on " + flow},
			map[string]any{"type": "tool_use", "name": "Write", "input": map[string]any{"file_path": "fixed.txt"}},
		}}})
	}
	switch mode {
	case "spawn":
		self, _ := os.Executable()
		child := exec.Command(self) //nolint:gosec,noctx // test helper
		child.Env = append(os.Environ(), "FAKECLI_MODE=sleep")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr // an MCP-server-like straggler holding our pipes
		if err := child.Start(); err == nil {
			_ = os.WriteFile(os.Getenv("FAKECLI_RECORD")+".pid", []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
	case "fail":
		fmt.Fprintln(os.Stderr, "fake failure")
		os.Exit(1)
	case "authfail": // claude with an expired sign-in, as claude 2.x reports it
		msg := "Failed to authenticate: OAuth session expired and could not be refreshed"
		emit(map[string]any{"type": "assistant", "error": "authentication_failed", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": msg}}}})
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "num_turns": 1, "result": msg})
		os.Exit(1)
	case "hang":
		self, _ := os.Executable()
		child := exec.Command(self) //nolint:gosec,noctx // test helper
		child.Env = append(os.Environ(), "FAKECLI_MODE=sleep")
		if err := child.Start(); err == nil {
			_ = os.WriteFile(os.Getenv("FAKECLI_RECORD")+".pid", []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
		time.Sleep(10 * time.Minute)
		return
	}
	var result map[string]any
	switch flow {
	case "reply":
		result = map[string]any{"reply": "Thanks for the report! Fixed in the next release.", "notes": ""}
	case "review":
		result = map[string]any{"verdict": "ok", "summary": "looks right"}
	case "label":
		picks := []string{}
		if v := os.Getenv("FAKECLI_LABELS"); v != "" {
			picks = strings.Split(v, ",")
		}
		result = map[string]any{"labels": picks, "summary": "picked from the list"}
	case "triage":
		result = map[string]any{"picks": triagePicks(string(stdin)), "summary": "ranked the open issues"}
	case "fix-direct":
		result = directFix(mode, args, string(stdin))
	case "fix-folder": // edit in place, no git
		if mode == "noop" {
			result = map[string]any{"status": "not_reproduced", "summary": "cannot reproduce", "files": []string{}, "verify": "", "notes": ""}
			break
		}
		if err := os.WriteFile("fixed.txt", []byte("fixed\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		_ = os.Remove("obsolete.txt")
		result = map[string]any{"status": "fixed", "summary": "wrote fixed.txt", "files": []string{"fixed.txt"}, "verify": "none", "notes": ""}
	default:
		if mode == "noop" {
			result = map[string]any{"status": "cannot_fix", "summary": "not reproducible", "notes": ""}
		} else {
			if err := os.WriteFile("fixed.txt", []byte("fixed\n"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			result = map[string]any{"status": "fixed", "summary": "wrote fixed.txt", "notes": ""}
		}
	}
	if codex {
		b, _ := json.Marshal(result)
		if i := slices.Index(args, "-o"); i >= 0 && i+1 < len(args) {
			_ = os.WriteFile(args[i+1], b, 0o600)
		}
		emit(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}})
		return
	}
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 2, "total_cost_usd": 0.01,
		"result": "", "structured_output": result})
}

// directFix edits and commits fixed.txt in the current folder (the mapped clone).
func directFix(mode string, args []string, stdin string) map[string]any {
	if mode == "noop" {
		return map[string]any{"status": "not_reproduced", "summary": "cannot reproduce", "commits": []string{}, "verify": "", "notes": ""}
	}
	prompt := stdin
	if i := slices.Index(args, "--append-system-prompt-file"); i >= 0 && i+1 < len(args) {
		b, _ := os.ReadFile(args[i+1])
		prompt = string(b)
	}
	n := "0"
	if m := regexp.MustCompile(`issue #(\d+)`).FindStringSubmatch(prompt); m != nil {
		n = m[1]
	}
	if err := os.WriteFile("fixed.txt", []byte("fixed "+n+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	msg := "fix: crash on start\n\nFixes #" + n
	if mode == "nofixes" {
		msg = "fix: crash on start"
	}
	git := func(a ...string) string {
		out, err := exec.Command("git", a...).CombinedOutput() //nolint:gosec,noctx // test helper
		if err != nil {
			fmt.Fprintln(os.Stderr, string(out), err)
			os.Exit(3)
		}
		return strings.TrimSpace(string(out))
	}
	git("add", "fixed.txt")
	git("commit", "-q", "-m", msg)
	sha := git("rev-parse", "HEAD")
	return map[string]any{"status": "fixed", "summary": "wrote fixed.txt", "commits": []string{sha}, "verify": "none", "notes": ""}
}

// triagePicks answers FAKECLI_PICKS, or every "#N " line of the prompt.
func triagePicks(prompt string) []map[string]any {
	picks := []map[string]any{}
	if v := os.Getenv("FAKECLI_PICKS"); v != "" {
		for _, p := range strings.Split(v, ",") {
			num, sev, _ := strings.Cut(p, ":")
			n, _ := strconv.Atoi(num)
			picks = append(picks, map[string]any{"number": n, "severity": sev, "reason": "fake reason " + num})
		}
		return picks
	}
	for _, m := range regexp.MustCompile(`(?m)^#(\d+) `).FindAllStringSubmatch(prompt, -1) {
		n, _ := strconv.Atoi(m[1])
		picks = append(picks, map[string]any{"number": n, "severity": "high", "reason": "listed as #" + m[1]})
	}
	return picks
}
