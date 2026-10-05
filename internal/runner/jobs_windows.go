package runner

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// liveTrees are the process trees attach created that are not closed yet: the
// agent CLIs (and their git calls) the runner started. InAgentJob checks a
// caller's process against them.
var liveTrees = struct {
	sync.Mutex
	m map[*procTree]struct{}
}{m: map[*procTree]struct{}{}}

func registerTree(p *procTree) {
	liveTrees.Lock()
	liveTrees.m[p] = struct{}{}
	liveTrees.Unlock()
}

func unregisterTree(p *procTree) {
	liveTrees.Lock()
	delete(liveTrees.m, p)
	liveTrees.Unlock()
}

// IsProcessInJob is not wrapped by golang.org/x/sys/windows.
var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func isProcessInJob(proc, job windows.Handle) (bool, error) {
	// BOOL out parameter.
	var in int32
	if r, _, err := procIsProcessInJob.Call(uintptr(proc), uintptr(job), uintptr(unsafe.Pointer(&in))); r == 0 { //nolint:gosec // G103: BOOL out parameter
		return false, err
	}
	return in != 0, nil
}

// InAgentJob reports whether process pid runs inside a live runner job object
// (an agent the runner started, or anything that agent spawned). A process
// that cannot be opened (already gone) is not in one.
func InAgentJob(pid uint32) bool {
	if pid == 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	liveTrees.Lock()
	trees := make([]*procTree, 0, len(liveTrees.m))
	for p := range liveTrees.m {
		trees = append(trees, p)
	}
	liveTrees.Unlock()
	for _, p := range trees {
		p.mu.Lock()
		in := false
		if p.job != 0 {
			in, _ = isProcessInJob(h, p.job)
		}
		p.mu.Unlock()
		if in {
			return true
		}
	}
	return false
}
