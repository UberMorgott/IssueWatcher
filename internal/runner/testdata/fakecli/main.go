// Command fakecli stands in for the claude and codex CLIs in runner tests. It
// speaks the same event formats (stream-json / --json), reads the prompt from
// stdin and acts by FAKECLI_MODE:
//
//	ok    edit fixed.txt (write flows), report a structured result, exit 0
//	noop  report "cannot_fix" without touching files, exit 0
//	fail  emit a step, exit 1
//	hang  start a child that sleeps, write its pid to FAKECLI_RECORD.pid, sleep
//	sleep sleep (the child of hang)
//
// FAKECLI_RECORD (a file) receives {args, stdin, ghToken} for assertions.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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
		b, _ := json.Marshal(map[string]any{"args": args, "stdin": string(stdin), "ghToken": os.Getenv("GH_TOKEN")})
		_ = os.WriteFile(rec, b, 0o600)
	}
	flow := "fix"
	schema := strings.Join(args, " ")
	if i := slices.Index(args, "--output-schema"); i >= 0 && i+1 < len(args) {
		b, _ := os.ReadFile(args[i+1])
		schema = string(b)
	}
	switch {
	case strings.Contains(schema, `"reply"`):
		flow = "reply"
	case strings.Contains(schema, `"verdict"`):
		flow = "review"
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
	case "fail":
		fmt.Fprintln(os.Stderr, "fake failure")
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
