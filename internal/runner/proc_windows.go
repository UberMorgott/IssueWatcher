package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// procTree holds a started command and everything it spawns in a Windows Job
// Object (kill-on-close), so kill ends the whole tree: the CLI, its node
// children, MCP servers (aegis, serena), shells, test runners.
type procTree struct {
	job windows.Handle
}

// prepare hides a short helper command (git, --version); it is not put in a job.
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | windows.CREATE_NEW_PROCESS_GROUP}
}

// prepareTree is prepare for a command attach will own: it starts suspended, so
// nothing it spawns can escape before it is in the job; attach resumes it.
func prepareTree(cmd *exec.Cmd) {
	prepare(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// attach creates a kill-on-close job object, assigns the (suspended) started
// process and resumes it.
func attach(cmd *exec.Cmd) (*procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("runner: job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { //nolint:gosec // G103: the documented way to pass the limits struct
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runner: job object limits: %w", err)
	}
	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a PID we just started
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runner: open process: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runner: assign job object: %w", err)
	}
	if err := resume(pid); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &procTree{job: job}, nil
}

// resume resumes the threads of a process started with CREATE_SUSPENDED (os/exec
// does not keep the thread handle).
func resume(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("runner: thread snapshot: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			continue
		}
		if _, err := windows.ResumeThread(th); err == nil {
			resumed++
		}
		_ = windows.CloseHandle(th)
	}
	if resumed == 0 {
		return errors.New("runner: could not resume the started process")
	}
	return nil
}

// kill terminates every process in the tree.
func (p *procTree) kill() {
	if p != nil && p.job != 0 {
		_ = windows.TerminateJobObject(p.job, 1)
	}
}

// jobAccounting mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	TotalUserTime, TotalKernelTime, ThisPeriodUserTime, ThisPeriodKernelTime int64
	TotalPageFaults, TotalProcesses, ActiveProcesses, TotalTerminated        uint32
}

// active counts the processes still alive in the tree (-1 = unknown).
func (p *procTree) active() int {
	if p == nil || p.job == 0 {
		return 0
	}
	var info jobAccounting
	if err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: documented struct pass
		return -1
	}
	return int(info.ActiveProcesses)
}

// pids lists the processes in the tree now.
func (p *procTree) pids() []uint32 {
	if p == nil || p.job == 0 {
		return nil
	}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST: two DWORD counts, then ULONG_PTR ids.
	var buf struct {
		Assigned, Listed uint32
		IDs              [256]uintptr
	}
	if err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf)), uint32(unsafe.Sizeof(buf)), nil); err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) { //nolint:gosec // G103: documented struct pass
		return nil
	}
	out := make([]uint32, 0, buf.Listed)
	for _, id := range buf.IDs[:min(int(buf.Listed), len(buf.IDs))] {
		out = append(out, uint32(id)) //nolint:gosec // G115: process ids fit a DWORD
	}
	return out
}

// end terminates the whole tree (a normal exit included: stragglers such as MCP
// servers die with it), waits until no process is left and releases the job.
// The error names processes that were still alive after wait.
func (p *procTree) end(wait time.Duration) error {
	if p == nil || p.job == 0 {
		return nil
	}
	defer p.close()
	p.kill()
	for deadline := time.Now().Add(wait); p.active() > 0; {
		if time.Now().After(deadline) {
			return fmt.Errorf("processes still alive after the job ended: %v", p.pids())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// close releases the job object (kill-on-close ends stragglers).
func (p *procTree) close() {
	if p != nil && p.job != 0 {
		_ = windows.CloseHandle(p.job)
		p.job = 0
	}
}

// processCreated returns the creation time of process pid (100 ns since 1601),
// which tells a recorded process from a later one that reuses its pid.
func processCreated(pid uint32) (int64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, false
	}
	return int64(created.HighDateTime)<<32 | int64(created.LowDateTime), true
}

// killRecorded terminates the recorded processes that still run (same pid and
// creation time) and their descendants; it returns how many it killed.
func killRecorded(recs []procRecord) int {
	type proc struct {
		parent  uint32
		created int64
	}
	all := map[uint32]proc{}
	if snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0); err == nil {
		pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
		for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
			if c, ok := processCreated(pe.ProcessID); ok {
				all[pe.ProcessID] = proc{parent: pe.ParentProcessID, created: c}
			}
		}
		_ = windows.CloseHandle(snap)
	}
	var kill []uint32
	for _, r := range recs {
		if p, ok := all[r.PID]; ok && p.created == r.Created && !slices.Contains(kill, r.PID) {
			kill = append(kill, r.PID)
		}
	}
	// Descendants: a child is younger than its parent (older = pid reuse).
	for i := 0; i < len(kill); i++ {
		parent := all[kill[i]]
		for pid, p := range all {
			if p.parent == kill[i] && p.created >= parent.created && !slices.Contains(kill, pid) {
				kill = append(kill, pid)
			}
		}
	}
	n := 0
	for _, pid := range kill {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			continue
		}
		var created, exited, kernel, user windows.Filetime
		if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil &&
			int64(created.HighDateTime)<<32|int64(created.LowDateTime) == all[pid].created &&
			windows.TerminateProcess(h, 1) == nil {
			n++
		}
		_ = windows.CloseHandle(h)
	}
	return n
}
