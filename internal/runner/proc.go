package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// procsFile (in a job's folder) records the processes of the running tree with
// their creation times, so the next start can kill what a crash left behind
// without hitting a process that later reused a pid.
const procsFile = "procs.json"

// procRecord is one process of a job's tree.
type procRecord struct {
	PID     uint32 `json:"pid"`
	Created int64  `json:"created"` // creation time (Windows FILETIME)
}

// Grace periods when a tree ends: processes to die, pipes to drain.
const (
	treeEndWait  = 10 * time.Second
	pipeDrainMax = 5 * time.Second
)

// proc is a started command in its own process tree. Its output arrives through
// pipes the runner owns (not os/exec copies), so a grandchild that inherited
// them cannot keep the run alive after the command exits: the tree is ended and
// the pipes close.
type proc struct {
	cmd     *exec.Cmd
	tree    *procTree
	done    chan error // cmd.Wait: the main process exited
	readers sync.WaitGroup
}

// startProc starts cmd in a new process tree with stdin text and one callback
// per output line (each stream read by one goroutine).
func startProc(cmd *exec.Cmd, stdin string, stdout, stderr func([]byte)) (p *proc, err error) {
	var files []*os.File
	pipe := func() (r, w *os.File) {
		if err != nil {
			return nil, nil
		}
		r, w, err = os.Pipe()
		files = append(files, r, w)
		return r, w
	}
	inR, inW := pipe()
	outR, outW := pipe()
	errR, errW := pipe()
	closeAll := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	if err != nil {
		closeAll()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	prepareTree(cmd)
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	// The child holds its own copies now.
	_, _, _ = inR.Close(), outW.Close(), errW.Close()
	tree, err := attach(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_, _, _ = inW.Close(), outR.Close(), errR.Close()
		return nil, err
	}
	p = &proc{cmd: cmd, tree: tree, done: make(chan error, 1)}
	go func() {
		_, _ = io.WriteString(inW, stdin)
		_ = inW.Close()
	}()
	for _, x := range []struct {
		r *os.File
		f func([]byte)
	}{{outR, stdout}, {errR, stderr}} {
		p.readers.Go(func() {
			readLines(x.r, x.f)
			_ = x.r.Close()
		})
	}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

// finish ends the tree after the main process exited (or was killed): every
// remaining descendant is terminated, the output drained. The error reports
// processes that survived (none should: the job object kills them all).
func (p *proc) finish() error {
	err := p.tree.end(treeEndWait)
	drained := make(chan struct{})
	go func() { p.readers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(pipeDrainMax):
		err = errors.Join(err, errors.New("output pipes still open after the process tree ended"))
	}
	return err
}

// trackProcs records the tree's processes in file every 2 s until stop, which
// removes the file (the tree is gone by then).
func trackProcs(tree *procTree, file string) (stop func()) {
	seen := map[uint32]int64{}
	save := func() {
		changed := false
		for _, pid := range tree.pids() {
			if _, ok := seen[pid]; ok {
				continue
			}
			if c, ok := processCreated(pid); ok {
				seen[pid], changed = c, true
			}
		}
		if !changed {
			return
		}
		recs := make([]procRecord, 0, len(seen))
		for pid, c := range seen {
			recs = append(recs, procRecord{PID: pid, Created: c})
		}
		b, _ := json.Marshal(recs) // plain structs
		_ = os.WriteFile(file, b, 0o600)
	}
	save()
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				save()
			}
		}
	}()
	return func() {
		close(quit)
		<-done
		_ = os.Remove(file)
	}
}

// killOrphans ends process trees a crashed previous run left behind (recorded
// in data\jobs\*\procs.json) and removes the records.
func killOrphans(dataDir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dataDir, stepsDirName, "*", procsFile))
	if err != nil {
		return 0, err
	}
	killed := 0
	var errs []error
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: our own job folder
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var recs []procRecord
		if err := json.Unmarshal(b, &recs); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
		}
		killed += killRecorded(recs)
		if err := os.Remove(f); err != nil {
			errs = append(errs, err)
		}
	}
	return killed, errors.Join(errs...)
}
