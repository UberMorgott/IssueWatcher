package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Step kinds shown in the job log.
const (
	StepInfo   = "info"   // dispatcher: preparing, git, verify, publish
	StepText   = "text"   // agent message
	StepTool   = "tool"   // agent tool call / command
	StepOutput = "output" // tool result / command output (clipped)
	StepError  = "error"
	StepStderr = "stderr" // CLI stderr line
	StepResult = "result" // agent finished (cost, turns)
)

// Step is one human-readable line of a job log.
type Step struct {
	T    time.Time `json:"t"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

const (
	maxStepText  = 4000
	maxLogSteps  = 5000 // per attempt; later steps are counted, not stored
	stepsDirName = "jobs"
)

// jobLog appends the steps of one attempt to data\jobs\<id>\<attempt>.log (JSONL).
type jobLog struct {
	mu      sync.Mutex
	f       *os.File
	n       int
	dropped int
	emit    func(Step)
}

func jobDir(dataDir string, id int64) string {
	return filepath.Join(dataDir, stepsDirName, strconv.FormatInt(id, 10))
}

func logPath(dataDir string, id int64, attempt int) string {
	return filepath.Join(jobDir(dataDir, id), strconv.Itoa(attempt)+".log")
}

func openJobLog(dataDir string, id int64, attempt int, emit func(Step)) (*jobLog, error) {
	if err := os.MkdirAll(jobDir(dataDir, id), 0o750); err != nil {
		return nil, fmt.Errorf("runner: log dir: %w", err)
	}
	f, err := os.OpenFile(logPath(dataDir, id, attempt), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runner: open log: %w", err)
	}
	return &jobLog{f: f, emit: emit}, nil
}

func (l *jobLog) add(kind, text string) {
	text = strings.TrimRight(text, "\r\n ")
	if text == "" {
		return
	}
	if r := []rune(text); len(r) > maxStepText {
		text = string(r[:maxStepText]) + " …"
	}
	st := Step{T: time.Now().UTC(), Kind: kind, Text: text}
	l.mu.Lock()
	if l.n >= maxLogSteps {
		l.dropped++
		l.mu.Unlock()
		return
	}
	l.n++
	if l.f != nil {
		b, _ := json.Marshal(st) // plain struct
		_, _ = l.f.Write(append(b, '\n'))
	}
	l.mu.Unlock()
	if l.emit != nil {
		l.emit(st)
	}
}

func (l *jobLog) addf(kind, format string, a ...any) { l.add(kind, fmt.Sprintf(format, a...)) }

func (l *jobLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dropped > 0 && l.f != nil {
		b, _ := json.Marshal(Step{T: time.Now().UTC(), Kind: StepInfo, Text: fmt.Sprintf("… %d more steps not kept", l.dropped)})
		_, _ = l.f.Write(append(b, '\n'))
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// ReadLog returns the steps of attempt of job id (empty when there is none).
func ReadLog(dataDir string, id int64, attempt int) ([]Step, error) {
	f, err := os.Open(logPath(dataDir, id, attempt))
	if errors.Is(err, os.ErrNotExist) {
		return []Step{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runner: read log: %w", err)
	}
	defer func() { _ = f.Close() }()
	out := []Step{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var st Step
		if json.Unmarshal(sc.Bytes(), &st) == nil {
			out = append(out, st)
		}
	}
	return out, sc.Err()
}
